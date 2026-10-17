package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

func contractAnalysisRegistryFixture(path string) graph.Reader {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "http::GET::route", Name: "http::GET::route", Kind: graph.KindContract, RepoPrefix: "repo", FilePath: "repo/handler.go",
		Meta: map[string]any{"type": "http", "role": "provider", "contract_meta": map[string]any{"path": path, "method": "GET"}}})
	return g
}

func TestContractAnalysisContextUsesSelectedRegistryAndKnownZero(t *testing.T) {
	primary := contracts.NewRegistry()
	primary.Add(contracts.Contract{ID: "primary", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, RepoPrefix: "repo", Meta: map[string]any{"path": "/primary"}})
	s := &Server{contractRegistry: primary}
	binding := &contractAnalysisContext{views: map[string]*graphview.ContractAnalysisView{"repo": {RegistryReader: contractAnalysisRegistryFixture("/selected")}}}
	defer binding.close()
	ctx := withContractAnalysisContext(context.Background(), binding)
	reg, err := s.contractRegistryForContext(ctx)
	require.NoError(t, err)
	require.Len(t, reg.All(), 1)
	require.Equal(t, "/selected", reg.All()[0].Meta["path"])
	require.Equal(t, "/primary", primary.All()[0].Meta["path"])
	empty := &contractAnalysisContext{views: map[string]*graphview.ContractAnalysisView{"repo": {RegistryReader: graph.New()}}}
	reg, err = s.contractRegistryForContext(withContractAnalysisContext(context.Background(), empty))
	require.NoError(t, err)
	require.NotNil(t, reg, "certified empty attachment must remain distinguishable from missing analysis")
	require.Empty(t, reg.All())
}

type failedContractAnalysisRegistry struct {
	graph.Reader
	failure error
}

func (r failedContractAnalysisRegistry) LoadContractRepoProjectionContext(context.Context, string) (graph.ContractFileProjection, error) {
	return graph.ContractFileProjection{ScalarNodes: []*graph.Node{{ID: "partial", Kind: graph.KindContract}}}, r.failure
}

func TestContractAnalysisContextNeverFallsBackAfterSelectedReadFailure(t *testing.T) {
	s := &Server{contractRegistry: contracts.NewRegistry()}
	failure := errors.New("selected payload read failed")
	binding := &contractAnalysisContext{views: map[string]*graphview.ContractAnalysisView{"repo": {RegistryReader: failedContractAnalysisRegistry{Reader: graph.New(), failure: failure}}}}
	reg, err := s.contractRegistryForContext(withContractAnalysisContext(context.Background(), binding))
	require.ErrorIs(t, err, failure)
	require.Nil(t, reg)
	reg, err = s.contractRegistryForContext(withRequestView(context.Background(), &requestView{reader: graph.New()}))
	require.Error(t, err, "an unbound worktree cannot borrow the primary registry")
	require.Nil(t, reg)
}

func TestContractAnalysisContextCapturesShapeReadFailure(t *testing.T) {
	binding := &contractAnalysisContext{views: map[string]*graphview.ContractAnalysisView{"repo": {}}}
	lookup := binding.shapeLookup(context.Background())
	require.Nil(t, lookup("repo/types.go::Response"))
	require.ErrorIs(t, binding.readError(), graph.ErrContractProjectionUnsupported, "errorless ShapeLookup must not erase checked read failure")
	require.ErrorIs(t, binding.validate(context.Background()), graph.ErrContractProjectionUnsupported, "late freshness validation must refuse a swallowed shape failure")
}
