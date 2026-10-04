package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

type contractCoreKindSpy struct {
	graph.Reader
	kindReads, fullReads int
	err                  error
}

func (s *contractCoreKindSpy) GetNodeKindsByIDsContext(ctx context.Context, ids []string) (map[string]graph.NodeKindRow, error) {
	s.kindReads++
	if s.err != nil {
		return nil, s.err
	}
	return graph.GetNodeKindsByIDsContext(ctx, s.Reader, ids)
}
func (s *contractCoreKindSpy) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.fullReads++
	return s.Reader.GetNodesByIDs(ids)
}

func TestContractCoreEdgesClassifyEndpointKindsWithoutFullNodes(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "caller", Kind: graph.KindFunction}, {ID: "callee", Kind: graph.KindFunction},
		{ID: "contract", Kind: graph.KindContract}, {ID: "table", Kind: graph.KindTable},
	}, []*graph.Edge{
		{From: "caller", To: "callee", Kind: graph.EdgeCalls},
		{From: "caller", To: "callee", Kind: graph.EdgeCalls, Line: 2, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "caller", To: "contract", Kind: graph.EdgeProvides},
		{From: "caller", To: "table", Kind: graph.EdgeProvides},
	})
	spy := &contractCoreKindSpy{Reader: base}
	reader := newContractCoreEdges(spy, withContractCoreReadErrors(t.Context()), nil)
	rows := reader.GetOutEdges("caller")
	require.Len(t, rows, 2)
	for _, edge := range rows {
		require.NotEqual(t, "contract", edge.To)
		require.NotEqual(t, "spring.Bean", edge.Meta["via"])
	}
	require.Equal(t, 1, spy.kindReads)
	require.Zero(t, spy.fullReads)
	batch := reader.GetInEdgesByNodeIDs([]string{"callee", "table"})
	require.Len(t, batch["callee"], 1)
	require.Len(t, batch["table"], 1)
	require.Equal(t, 2, spy.kindReads, "one structural endpoint batch per adjacency batch")
	var sqlEdges int
	for edge := range reader.EdgesByKind(graph.EdgeProvides) {
		require.Equal(t, "table", edge.To)
		sqlEdges++
	}
	require.Equal(t, 1, sqlEdges)
	require.Zero(t, spy.fullReads)
}

func installContractCoreKindTestRuntime(t *testing.T, srv *Server) {
	t.Helper()
	srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{
		Request: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error) {
			t.Error("ordinary core request scheduled contract work")
			return false, nil
		},
		WaitChange: func(context.Context, graph.ContractAttachmentKey) error {
			t.Error("ordinary core request waited for contract work")
			return nil
		},
	})
	require.NotNil(t, srv.contractAnalysisRuntime)
}

func TestContractCoreKindReadFailureRefusesHandlerAnswer(t *testing.T) {
	srv, _ := setupTestServer(t)
	installContractCoreKindTestRuntime(t, srv)
	base := graph.New()
	base.AddBatch([]*graph.Node{{ID: "caller", Kind: graph.KindFunction}, {ID: "callee", Kind: graph.KindFunction}}, []*graph.Edge{{From: "caller", To: "callee", Kind: graph.EdgeCalls}})
	sentinel := errors.New("endpoint kind projection unavailable")
	spy := &contractCoreKindSpy{Reader: base, err: sentinel}
	request := mcpsdk.CallToolRequest{}
	request.Params.Name = "get_callers"
	request.Params.Arguments = map[string]any{"id": "callee"}
	var observed []*graph.Edge
	result, err := srv.wrapToolHandler(func(ctx context.Context, _ mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		reader := newContractCoreEdges(spy, ctx, nil)
		observed = reader.GetInEdges("callee")
		return mcpsdk.NewToolResultText("no callers"), nil
	})(t.Context(), request)
	require.NoError(t, err)
	require.Empty(t, observed)
	require.Equal(t, 1, spy.kindReads)
	require.True(t, result.IsError, "failed structural proof must not certify no callers")
	body := result.Content[0].(mcpsdk.TextContent).Text
	require.Contains(t, body, sentinel.Error())
	require.NotContains(t, body, "no callers")
}

func TestContractCoreKindProjectionPreservesBaseRepositoryScope(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/source.go"},
		{ID: "b", Kind: graph.KindContract, RepoPrefix: "b", FilePath: "b/source.go"},
		{ID: "legacy", Kind: graph.KindTable, FilePath: "a/legacy.sql"},
	}, nil)
	reader := newBaseGraphReader(base, "a")
	rows, err := graph.GetNodeKindsByIDsContext(t.Context(), reader, []string{"a", "b", "legacy"})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, graph.KindFunction, rows["a"].Kind)
	require.Equal(t, graph.KindTable, rows["legacy"].Kind)
	require.NotContains(t, rows, "b")
}

func TestContractCoreKindGuardPreservesCommittedMutationReceipt(t *testing.T) {
	srv, _ := setupTestServer(t)
	installContractCoreKindTestRuntime(t, srv)
	base := graph.New()
	base.AddBatch([]*graph.Node{{ID: "caller", Kind: graph.KindFunction}, {ID: "callee", Kind: graph.KindFunction}}, []*graph.Edge{{From: "caller", To: "callee", Kind: graph.EdgeCalls}})
	sentinel := errors.New("late endpoint read failure")
	spy := &contractCoreKindSpy{Reader: base, err: sentinel}
	path := filepath.Join(t.TempDir(), "committed.txt")
	request := mcpsdk.CallToolRequest{}
	request.Params.Name = "write_file"
	request.Params.Arguments = map[string]any{"path": path, "content": "committed"}
	var observed []*graph.Edge
	result, err := srv.wrapToolHandler(func(ctx context.Context, _ mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		if err := os.WriteFile(path, []byte("committed"), 0600); err != nil {
			return nil, err
		}
		observed = newContractCoreEdges(spy, ctx, nil).GetInEdges("callee")
		// Even a late recorded read error cannot withdraw a mutation receipt.
		recordContractCoreReadError(ctx, sentinel)
		return mcpsdk.NewToolResultText("committed mutation receipt"), nil
	})(t.Context(), request)
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Contains(t, result.Content[0].(mcpsdk.TextContent).Text, "committed mutation receipt")
	require.Len(t, observed, 1)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "committed", string(content))
	require.Zero(t, spy.kindReads, "writers must retain the previous classification path")
	require.Equal(t, 1, spy.fullReads)
}

func TestContractCoreKindGuardPreservesHandlerErrors(t *testing.T) {
	for _, transportError := range []bool{false, true} {
		t.Run(map[bool]string{false: "error_result", true: "handler_error"}[transportError], func(t *testing.T) {
			srv, _ := setupTestServer(t)
			installContractCoreKindTestRuntime(t, srv)
			request := mcpsdk.CallToolRequest{}
			request.Params.Name = "get_callers"
			sentinel := errors.New("original handler failure")
			result, err := srv.wrapToolHandler(func(ctx context.Context, _ mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				recordContractCoreReadError(ctx, errors.New("secondary projection failure"))
				if transportError {
					return nil, sentinel
				}
				return mcpsdk.NewToolResultError(sentinel.Error()), nil
			})(t.Context(), request)
			if transportError {
				require.ErrorIs(t, err, sentinel)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.True(t, result.IsError)
				require.Contains(t, result.Content[0].(mcpsdk.TextContent).Text, sentinel.Error())
			}
		})
	}
}
