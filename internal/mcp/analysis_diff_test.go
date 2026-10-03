package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

type diffMembershipStore struct {
	*store_sqlite.Store
	t      *testing.T
	calls  int
	cancel context.CancelFunc
}

func (s *diffMembershipStore) ListAnalysisCommunitySummaries(int64, int, string) ([]graph.AnalysisCommunitySummary, string, error) {
	s.t.Fatal("diff entered the whole-generation community summary loop")
	return nil, "", nil
}

func (s *diffMembershipStore) AnalysisCommunityMembers(int64, string, int, string) ([]graph.AnalysisNodeMetric, string, error) {
	s.t.Fatal("diff entered the whole-generation community member loop")
	return nil, "", nil
}

func (s *diffMembershipStore) ListAnalysisProcessSummaries(int64, int, string) ([]graph.AnalysisProcessSummary, string, error) {
	s.t.Fatal("diff entered the whole-generation process summary loop")
	return nil, "", nil
}

func (s *diffMembershipStore) AnalysisProcessSteps(int64, string, int, int) ([]graph.AnalysisProcessStep, int, error) {
	s.t.Fatal("diff entered the whole-generation process step loop")
	return nil, -1, nil
}

func (s *diffMembershipStore) AnalysisMembershipsContext(ctx context.Context, generationID int64, ids []string) ([]graph.AnalysisMembership, error) {
	s.calls++
	require.LessOrEqual(s.t, len(ids), analysisGenerationQueryMax)
	rows, err := s.Store.AnalysisMembershipsContext(ctx, generationID, ids)
	if s.cancel != nil {
		s.cancel()
	}
	return rows, err
}

func TestDiffConsumersMatchFullViewsWithoutHydration(t *testing.T) {
	dir, a, b, c := siblingDiffGitRepo(t)
	t.Chdir(dir)
	store, err := store_sqlite.Open(t.TempDir() + "/analysis.sqlite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "Alpha", Name: "Alpha", Kind: graph.KindFunction, FilePath: a, StartLine: 3, EndLine: 7, Language: "go"},
		{ID: "Beta", Name: "Beta", Kind: graph.KindFunction, FilePath: b, StartLine: 3, EndLine: 7, Language: "go"},
		{ID: "Gamma", Name: "Gamma", Kind: graph.KindFunction, FilePath: c, StartLine: 3, EndLine: 7, Language: "go"},
		{ID: "Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "caller.go", StartLine: 1, EndLine: 3, Language: "go"},
	}, []*graph.Edge{{From: "Caller", To: "Alpha", Kind: graph.EdgeCalls, FilePath: "caller.go", Line: 2}})
	server, metrics := populateAnalysisForTest(store)
	require.NoError(t, metrics.cacheSaveErr)
	communities, processes := server.communities, server.processes
	require.NotNil(t, communities)
	require.NotNil(t, processes)
	require.NotEmpty(t, communities.NodeToComm["Alpha"])
	require.NotEmpty(t, processes.NodeToProcs["Alpha"])
	counting := &diffMembershipStore{Store: store, t: t}
	server.graph = counting
	server.engine = query.NewEngine(counting)
	server.session = newSessionState()

	wantImpact := analysis.AnalyzeImpact(store, []string{"Alpha"}, communities, processes)
	require.NotEmpty(t, wantImpact.ByDepth[1])
	require.True(t, server.releaseTransientAnalysisIfIdle())
	gotImpact, err := server.analyzeDiffImpact(t.Context(), []string{"Alpha"})
	require.NoError(t, err)
	require.Equal(t, wantImpact, gotImpact)
	assertNoMaterializedAnalysis(t, server)

	for _, tc := range []struct {
		name    string
		handler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error)
		args    map[string]any
	}{
		{"detect", server.handleDetectChanges, map[string]any{"scope": "compare", "base_ref": "base-ref", "format": "json"}},
		{"diff_context", server.handleDiffContext, map[string]any{"scope": "compare", "base_ref": "base-ref", "format": "json"}},
		{"pr_context", server.handlePRReviewContext, map[string]any{"ids": "Alpha,Beta,Gamma", "sections": "diff_context", "verify": false, "audit_config": false, "format": "json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server.communities, server.processes = communities, processes
			req := mcplib.CallToolRequest{}
			req.Params.Arguments = tc.args
			before := counting.calls
			full, err := tc.handler(t.Context(), req)
			require.NoError(t, err)
			require.False(t, full.IsError, toolText(full))
			require.Contains(t, toolText(full), communities.NodeToComm["Alpha"])
			require.Contains(t, toolText(full), processes.NodeToProcs["Alpha"][0])
			require.Equal(t, before, counting.calls, "full views must be reused")
			require.True(t, server.releaseTransientAnalysisIfIdle())
			lazy, err := tc.handler(t.Context(), req)
			require.NoError(t, err)
			require.False(t, lazy.IsError, toolText(lazy))
			var want, got any
			require.NoError(t, json.Unmarshal([]byte(toolText(full)), &want))
			require.NoError(t, json.Unmarshal([]byte(toolText(lazy)), &got))
			require.Equal(t, want, got)
			require.Greater(t, counting.calls, before)
			assertNoMaterializedAnalysis(t, server)
		})
	}
}

func TestDiffMembershipCancellationStopsBetweenBatches(t *testing.T) {
	store, _ := buildAnalysisCacheTestGraph(t, 20)
	t.Cleanup(func() { _ = store.Close() })
	server, metrics := populateAnalysisForTest(store)
	require.NoError(t, metrics.cacheSaveErr)
	require.True(t, server.releaseTransientAnalysisIfIdle())
	counting := &diffMembershipStore{Store: store, t: t}
	server.graph = counting
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := server.analysisMembershipsForNodes(ctx, []string{"repo::pkg0::HandleRequest0"})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, counting.calls)

	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	counting.cancel = cancel
	ids := make([]string, analysisGenerationQueryMax+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("node-%d", i)
	}
	_, _, err = server.analysisMembershipsForNodes(ctx, ids)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, counting.calls, "cancellation must prevent the next batch")
	assertNoMaterializedAnalysis(t, server)
}
