package mcp

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestSearchSymbolsNativePathBundlesKeepDeepRecallAndCompleteCursor(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "path.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	var nodes []*graph.Node
	var items []graph.SymbolFTSItem
	for i := 0; i < 340; i++ {
		file := fmt.Sprintf("repo/noise/%03d.go", i)
		n := &graph.Node{ID: file + "::Noise", Name: "Noise", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: file}
		nodes = append(nodes, n)
		items = append(items, graph.SymbolFTSItem{NodeID: n.ID, Tokens: "output encoding output encoding"})
	}
	want := map[string]bool{}
	for i := 0; i < 17; i++ {
		file := fmt.Sprintf("repo/src/%03d.go", i)
		n := &graph.Node{ID: file + "::Format", Name: "Format", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: file}
		nodes = append(nodes, n)
		items = append(items, graph.SymbolFTSItem{NodeID: n.ID, Tokens: "output encoding"})
		want[n.ID] = true
	}
	require.NoError(t, store.AddBatchChecked(nodes, nil))
	require.NoError(t, store.BulkUpsertSymbolFTS("repo", items))
	eng := query.NewEngine(store)
	eng.SetSearch(search.NewSwappable(search.NewHybrid(search.NewSymbolSearcherBackend(store), nil, nil)))
	eng.SetRerank(nil)
	srv := NewServer(eng, store, nil, nil, zap.NewNop(), nil)
	off := false
	srv.SetSearchConfig(config.SearchConfig{RerankEmbedder: &off})
	srv.RunAnalysis()
	args := map[string]any{"query": "output encoding", "path": "./src/", "limit": 5, "expand": "none", "assist": "never"}
	seen := map[string]bool{}
	for page := 0; page < 20; page++ {
		resp := searchResp(t, srv, args)
		for id := range respIDs(resp) {
			require.True(t, want[id], id)
			require.False(t, seen[id], "duplicate page result %s", id)
			seen[id] = true
		}
		cursor, _ := resp["next_cursor"].(string)
		if cursor == "" {
			require.Equal(t, false, resp["truncated"])
			break
		}
		args["cursor"] = cursor
	}
	require.Equal(t, want, seen, "native pre-limit path filtering must not lose deep-ranked symbols")
	for _, path := range []string{"srcX", "absent/deleted.go"} {
		miss := searchResp(t, srv, map[string]any{"query": "output encoding", "path": path, "limit": 5, "expand": "none", "assist": "never"})
		require.Empty(t, respIDs(miss))
		require.Equal(t, false, miss["truncated"])
		require.Empty(t, miss["next_cursor"])
	}
}
