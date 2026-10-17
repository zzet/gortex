package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search/rerank"
)

func TestCentralityPresenceTimingUsesRealCoreAndPreservesScores(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "presence.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{{ID: "repo/a.go::A", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"}, {ID: "repo/b.go::B", Kind: graph.KindFunction, FilePath: "repo/b.go", RepoPrefix: "repo"}}, []*graph.Edge{{From: "repo/a.go::A", To: "repo/b.go::B", Kind: graph.EdgeCalls, Meta: map[string]any{"origin": "static"}}})
	server := &Server{graph: store}
	installContractCoreKindTestRuntime(t, server)
	ctx := withContractCoreReadErrors(t.Context())
	plain := server.boundedCentralityForRequest(ctx, []string{"repo/a.go::A"}, []string{"repo/a.go::A"})
	var timing rerank.CentralityTiming
	observed := server.boundedCentralityForRequestObserved(ctx, []string{"repo/a.go::A"}, []string{"repo/a.go::A"}, func(v rerank.CentralityTiming) { timing = v })
	require.Equal(t, plain, observed)
	require.NoError(t, contractCoreReadError(ctx))
	require.Positive(t, timing.NodePresence.Batches)
	require.Positive(t, timing.NodePresence.Rows)
	require.Zero(t, timing.NodePresence.Errors)
	require.GreaterOrEqual(t, timing.NodeRead, timing.NodePresence.Total)
	fields := symbolCentralityTimingFields(timing)["node_presence_read"].(map[string]any)
	require.Equal(t, timing.NodePresence.Batches, fields["batches"])
}
