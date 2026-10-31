package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// addSearchRowsRepo indexes repo's three Close methods, a caller of each and
// their symbol search tokens.
func addSearchRowsRepo(t *testing.T, store *store_sqlite.Store, repo string) {
	t.Helper()
	var nodes []*graph.Node
	var edges []*graph.Edge
	var items []graph.SymbolFTSItem
	for _, owner := range []string{"io/file.go::File", "net/conn.go::Conn", "db/pool.go::Pool"} {
		path, typeName, _ := strings.Cut(owner, "::")
		closeID := repo + "/" + owner + ".Close"
		caller := repo + "/" + path + "::shutdown" + typeName
		nodes = append(nodes,
			&graph.Node{ID: closeID, Name: "Close", Kind: graph.KindMethod, RepoPrefix: repo, FilePath: repo + "/" + path, Language: "go", StartLine: 10, EndLine: 14},
			&graph.Node{ID: caller, Name: "shutdown" + typeName, Kind: graph.KindFunction, RepoPrefix: repo, FilePath: repo + "/" + path, Language: "go", StartLine: 20, EndLine: 24},
		)
		edges = append(edges, &graph.Edge{From: caller, To: closeID, Kind: graph.EdgeCalls, FilePath: repo + "/" + path, Line: 22, Origin: graph.OriginASTResolved, Confidence: .9})
		items = append(items, graph.SymbolFTSItem{NodeID: closeID, Tokens: "close " + typeName})
	}
	require.NoError(t, store.AddBatchChecked(nodes, edges))
	require.NoError(t, store.BulkUpsertSymbolFTS(repo, items))
}

// A repo-scoped symbol search ranks inside the scoped repository's run of the
// symbol index, so every other repository defining the same Close methods
// changes neither the hits nor the graph rows read to hydrate them.
func TestSearchSymbolsScopedNeverScansAndRowsStayFlat(t *testing.T) {
	store := openContractCoreCapStore(t)
	addSearchRowsRepo(t, store, "a")
	counter := newScopedReadCounter(store)
	eng := query.NewEngine(counter)
	// The daemon searches the store itself: its repo-scoped ranking is
	// adapted from the concrete store, so the counter wraps only graph reads.
	eng.SetSearch(search.NewSwappable(search.NewHybrid(search.NewSymbolSearcherBackend(store), nil, nil)))
	eng.SetRerank(nil)
	srv := NewServer(eng, counter, nil, nil, zap.NewNop(), nil)
	off := false
	srv.SetSearchConfig(config.SearchConfig{RerankEmbedder: &off})
	installContractCoreKindTestRuntime(t, srv)
	find := func() string {
		hits := scopedRowCall(t, srv, srv.handleSearchSymbols, t.Context(), "search_symbols", map[string]any{"query": "Close", "repo": "a", "limit": 5, "expand": "none", "assist": "never", "format": "json"})
		narrow := scopedRowCall(t, srv, srv.handleSearchSymbols, t.Context(), "search_symbols", map[string]any{"query": "Close", "repo": "a", "path": "net", "limit": 5, "expand": "none", "assist": "never", "format": "json"})
		return "[" + hits + "," + narrow + "]"
	}
	var payload []map[string]any
	require.NoError(t, json.Unmarshal([]byte(find()), &payload))
	require.Len(t, respIDs(payload[0]), 3, "each of the scoped repository's Close methods is found")
	for id := range respIDs(payload[0]) {
		require.Regexp(t, `^a/`, id)
	}
	require.Len(t, respIDs(payload[1]), 1)
	requireScopedReadsStayFlat(t, counter, func() {
		for i := range 5 {
			addSearchRowsRepo(t, store, fmt.Sprintf("unrelated%d", i))
		}
	}, find)
}
