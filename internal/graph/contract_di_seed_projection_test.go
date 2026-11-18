package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContractDISeedProjectionKeepsSelectedAliasesAndForeignOwners(t *testing.T) {
	ctx := t.Context()
	base := New()
	local := &Node{ID: "repo/wire.go::Wire", Kind: KindFunction, RepoPrefix: "repo", FilePath: "repo/wire.go"}
	aliasSource := &Node{ID: "repo/alias.go::Alias", Kind: KindFunction, RepoPrefix: "repo", FilePath: "repo/alias.go"}
	foreign := &Node{ID: "other/wire.go::Wire", Kind: KindFunction, RepoPrefix: "other", FilePath: "other/wire.go"}
	target := &Node{ID: "di::TOKEN", Kind: KindContract, RepoPrefix: "other", Meta: map[string]any{"contract_meta": "shared"}}
	unused := &Node{ID: "http::GET::/unrelated", Kind: KindContract, RepoPrefix: "repo", FilePath: "repo/routes.go"}
	own := &Edge{From: local.ID, To: target.ID, Kind: EdgeProvides, FilePath: local.FilePath, Meta: map[string]any{MetaDIBinding: DIBindingUseValue, MetaDIToken: "TOKEN"}}
	other := &Edge{From: foreign.ID, To: target.ID, Kind: EdgeConsumes, FilePath: foreign.FilePath}
	base.AddBatch([]*Node{local, aliasSource, foreign, target, unused}, []*Edge{own, other})
	layer := NewOverlayLayer()
	alias := &Edge{From: aliasSource.ID, To: target.ID, Kind: EdgeProvides, FilePath: aliasSource.FilePath, Meta: map[string]any{MetaDIBinding: DIBindingUseExisting, MetaDIToken: "ALIAS"}}
	layer.AddEdge(alias)
	selected := NewOverlaidView(base, layer)
	full, err := CompleteContractRepoProjection(ctx, selected, "repo")
	require.NoError(t, err)
	require.NotEmpty(t, full.ScalarNodes)
	seeds, err := CompleteContractDISeedProjection(ctx, selected, "repo")
	require.NoError(t, err)
	require.Empty(t, seeds.ScalarNodes)
	require.ElementsMatch(t, full.OwnerRows, seeds.OwnerRows, "complete same-ID ownership retains the foreign sibling")
	require.Len(t, seeds.OwnerRows, 3)
	require.Equal(t, target, seeds.Targets[target.ID])
	require.Equal(t, foreign, seeds.SourceNodes[foreign.ID])
	removed := NewOverlayLayer()
	removed.MarkFile(local.FilePath, true)
	masked := NewOverlaidView(selected, removed)
	full, err = CompleteContractRepoProjection(ctx, masked, "repo")
	require.NoError(t, err)
	seeds, err = CompleteContractDISeedProjection(ctx, masked, "repo")
	require.NoError(t, err)
	require.ElementsMatch(t, full.OwnerRows, seeds.OwnerRows)
	require.Len(t, seeds.OwnerRows, 2, "the inherited local edge disappears, its alias and foreign sibling remain")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	seeds, err = CompleteContractDISeedProjection(canceled, selected, "repo")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, ContractFileProjection{}, seeds)
}

type repoOwnerAcceleratedGraph struct{ *Graph }

func (g *repoOwnerAcceleratedGraph) ContractRepoOwnerProjectionSource() any { return g }
func (g *repoOwnerAcceleratedGraph) LayerContractRepoOwnerProjectionContext(ctx context.Context, repo string) (ContractFileProjection, error) {
	return g.LayerContractRepoProjectionContext(ctx, repo)
}

type repoOwnerCheckedWrapper struct {
	*repoOwnerAcceleratedGraph
	// Deliberately makes the value receiver noncomparable.
	state   map[string]bool
	failure error
}

func (w repoOwnerCheckedWrapper) LayerContractRepoProjectionContext(context.Context, string) (ContractFileProjection, error) {
	return ContractFileProjection{}, w.failure
}

func TestContractDISeedProjectionDoesNotInheritAnAcceleratorsAuthority(t *testing.T) {
	failure := errors.New("checked selected revision moved")
	wrapper := repoOwnerCheckedWrapper{repoOwnerAcceleratedGraph: &repoOwnerAcceleratedGraph{New()}, state: map[string]bool{"selected": true}, failure: failure}
	for _, reader := range []Reader{wrapper, &wrapper} {
		p, err := CompleteContractDISeedProjection(t.Context(), reader, "repo")
		require.ErrorIs(t, err, failure, "a promoted accelerator must not skip the facade's checked revision error")
		require.Equal(t, ContractFileProjection{}, p)
	}
}

type fullOnlyRepoWrapper struct{ *Graph }

func TestContractDISeedProjectionKeepsFallbackScalarOwnershipGuards(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{{ID: "di::orphan", Kind: KindContract, RepoPrefix: "repo", FilePath: "repo/legacy.go"}}, []*Edge{{From: "missing-source", To: "di::orphan", Kind: EdgeProvides, FilePath: "other/recorded.go"}})
	for _, reader := range []Reader{g, &fullOnlyRepoWrapper{g}, NewOverlaidView(g, NewOverlayLayer())} {
		full, err := CompleteContractRepoProjection(t.Context(), reader, "repo")
		require.ErrorIs(t, err, ErrContractProjectionIncomplete)
		require.Equal(t, ContractFileProjection{}, full)
		seeds, err := CompleteContractDISeedProjection(t.Context(), reader, "repo")
		require.ErrorIs(t, err, ErrContractProjectionIncomplete, "an unaccelerated scalar key must still expose its missing owner")
		require.Equal(t, ContractFileProjection{}, seeds)
	}
}

func TestContractDISeedProjectionKeepsCrossKindCanonicalSeeds(t *testing.T) {
	g := New()
	router := &Node{ID: "repo::Router", Kind: KindFunction, RepoPrefix: "repo", FilePath: "repo/routes.go"}
	provider := &Node{ID: "foreign::Provider", Kind: KindFunction, RepoPrefix: "foreign", FilePath: "foreign/provider.go"}
	target := &Node{ID: "shared::TARGET", Kind: KindContract, RepoPrefix: "foreign"}
	route := &Edge{From: router.ID, To: target.ID, Kind: EdgeHandlesRoute, FilePath: router.FilePath}
	di := &Edge{From: provider.ID, To: target.ID, Kind: EdgeProvides, FilePath: provider.FilePath}
	g.AddBatch([]*Node{router, provider, target}, []*Edge{route, di})
	full, err := CompleteContractRepoProjection(t.Context(), g, "repo")
	require.NoError(t, err)
	require.Empty(t, full.ScalarNodes, "the shared contract is physically owned by the foreign namespace")
	require.Len(t, full.OwnerRows, 2, "the route key completes its foreign DI sibling")
	seeds, err := CompleteContractDISeedProjection(t.Context(), g, "repo")
	require.NoError(t, err)
	require.ElementsMatch(t, full.OwnerRows, seeds.OwnerRows)
	require.Equal(t, full.Targets, seeds.Targets)
	require.Equal(t, full.SourceNodes, seeds.SourceNodes)
}
