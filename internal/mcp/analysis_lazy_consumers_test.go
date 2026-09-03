package mcp

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/search/rerank"
)

func TestNormalAnalysisConsumersDoNotMaterializeWholeGraphMaps(t *testing.T) {
	store, _ := buildAnalysisCacheTestGraph(t, 80)
	defer store.Close()
	server, metrics := populateAnalysisForTest(store)
	if metrics.cacheSaveErr != nil {
		t.Fatalf("analysis generation save: %v", metrics.cacheSaveErr)
	}
	if !server.releaseTransientAnalysisIfIdle() {
		t.Fatal("analysis compatibility views were not released")
	}
	assertNoMaterializedAnalysis(t, server)

	id := "repo::pkg0::HandleRequest0"
	impact := server.analyzeImpactLazy(context.Background(), []string{id})
	if impact == nil {
		t.Fatal("lazy impact returned nil")
	}
	prediction := &prediction{
		changedIDs: []string{id},
		nodes:      server.nodesForIDs(context.Background(), []string{id}),
		impact:     impact,
	}
	_ = server.scoreChangeRisk(prediction)
	_ = server.riskGatedSymbols(context.Background(), prediction)
	assertNoMaterializedAnalysis(t, server)

	node := store.GetNode(id)
	rctx := server.buildRerankContext(context.Background(), "HandleRequest")
	rctx.Prepare([]*rerank.Candidate{{Node: node}})
	if receipt := rctx.CentralityTelemetry(); receipt.NodeCount == 0 {
		t.Fatalf("bounded centrality did not run: %+v", receipt)
	}
	assertNoMaterializedAnalysis(t, server)
}

func assertNoMaterializedAnalysis(t *testing.T, server *Server) {
	t.Helper()
	server.analysisMu.RLock()
	defer server.analysisMu.RUnlock()
	if server.communities != nil || server.processes != nil || server.pageRank != nil ||
		server.hits != nil || server.adjacency != nil || server.autoConcepts != nil ||
		server.leidenCache != nil {
		t.Fatalf("normal consumer materialized compatibility analysis: communities=%t processes=%t pagerank=%t hits=%t adjacency=%t concepts=%t leiden=%t",
			server.communities != nil, server.processes != nil, server.pageRank != nil,
			server.hits != nil, server.adjacency != nil, server.autoConcepts != nil,
			server.leidenCache != nil)
	}
}
