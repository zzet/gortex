package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

func TestNameOnlyCandidatesKeepFoldedLiteralsScopeAndRealHorizon(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("SQLite_%t", native), func(t *testing.T) {
			var store graph.Store = graph.New()
			if native {
				sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "literal-names.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, sql.Close()) })
				store = sql
			}
			for i, name := range []string{"Alpha", "AlphaTail", "XAlphaY", "Key", "Field_Name", "Field%Name", "Field*Name", "FieldXName"} {
				store.AddNode(&graph.Node{ID: fmt.Sprintf("selected/names.go::%d", i), Name: name, Kind: graph.KindFunction, FilePath: "selected/names.go", RepoPrefix: "selected"})
			}
			store.AddNode(&graph.Node{ID: "foreign/names.go::Alpha", Name: "Alpha", Kind: graph.KindFunction, FilePath: "foreign/names.go", RepoPrefix: "foreign"})
			scope := query.QueryOptions{RepoAllow: map[string]bool{"selected": true}}
			for _, tc := range []struct {
				needle string
				limit  int
				want   []string
				more   bool
			}{
				{"aLpHa", 2, []string{"Alpha", "AlphaTail"}, true},
				{"alpha", 3, []string{"Alpha", "AlphaTail", "XAlphaY"}, false},
				{"missing", 1, nil, false},
				{"kEY", 1, []string{"Key"}, false},
				{"field_name", 1, []string{"Field_Name"}, false},
				{"field%name", 1, []string{"Field%Name"}, false},
				{"field*name", 1, []string{"Field*Name"}, false},
			} {
				nodes, more, err := nameOnlySymbolCandidates(t.Context(), store, tc.needle, tc.limit, scope)
				require.NoError(t, err)
				var names []string
				for _, node := range nodes {
					require.Equal(t, "selected", node.RepoPrefix)
					names = append(names, node.Name)
				}
				require.Equal(t, tc.want, names, tc.needle)
				require.Equal(t, tc.more, more, tc.needle)
			}
		})
	}
}

func TestNameOnlyCursorKeepsActualMatchesAndHonestLateFilters(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "name-pages.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for i := range 20 {
		store.AddNode(&graph.Node{ID: fmt.Sprintf("scope/names.go::Window%02d", i), Name: fmt.Sprintf("Window%02d", i), Kind: graph.KindFunction, FilePath: "scope/names.go", Language: "go", StartLine: i + 1})
	}
	engine := query.NewEngine(store)
	engine.SetRerank(nil)
	server := NewServer(engine, store, nil, nil, zap.NewNop(), nil)
	args := func(cursor string) map[string]any {
		out := map[string]any{"query": "name:Window", "query_class": "symbol", "expand": "off", "limit": 2, "max_per_file": 0, "max_bytes": 0}
		if cursor != "" {
			out["cursor"] = cursor
		}
		return out
	}
	seen := map[string]bool{}
	cursor := ""
	for page := range 12 {
		result := callTool(t, server, "search_symbols", args(cursor))
		require.False(t, result.IsError, resultText(result))
		body := extractTextResult(t, result)
		rows, ok := body["results"].([]any)
		require.True(t, ok)
		for _, raw := range rows {
			id := raw.(map[string]any)["id"].(string)
			require.False(t, seen[id], "a cursor must not repeat a concrete declaration")
			seen[id] = true
		}
		next, _ := body["next_cursor"].(string)
		if next == "" {
			require.Equal(t, false, body["truncated"])
			break
		}
		require.Less(t, page, 11, "the fixed20-name population must exhaust")
		require.NotEqual(t, cursor, next)
		cursor = next
	}
	require.Len(t, seen, 20)
	late := args("")
	late["query"] = "name:Window lang:rust"
	body := extractTextResult(t, callTool(t, server, "search_symbols", late))
	require.Empty(t, body["results"])
	require.Equal(t, true, body["truncated"], "late language rejection must not manufacture literal-name exhaustion")
	require.NotEmpty(t, body["next_cursor"])
}

type nameCandidateContextStore struct {
	*store_sqlite.Store
	entered chan context.Context
	calls   atomic.Int32
}

func (s *nameCandidateContextStore) FindFoldedNameCandidatesContext(ctx context.Context, needle string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, bool, error) {
	s.calls.Add(1)
	s.entered <- ctx
	// A delegate-only barrier: real SQL pool admission/cancellation is covered
	// separately in store_sqlite, rather than mislabeling this as a pool wait.
	<-ctx.Done()
	return s.Store.FindFoldedNameCandidatesContext(ctx, needle, limit, filter)
}

func TestNameOnlyDelegationPreservesOriginalContextAndCancellation(t *testing.T) {
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "name-cancel.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	sql.AddNode(&graph.Node{ID: "main.go::Target", Name: "Target", Kind: graph.KindFunction, FilePath: "main.go"})
	store := &nameCandidateContextStore{Store: sql, entered: make(chan context.Context, 1)}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(time.Minute))
	original, known := ctx.Deadline()
	require.True(t, known)
	type answer struct {
		nodes []*graph.Node
		more  bool
		err   error
	}
	done := make(chan answer, 1)
	var worker sync.WaitGroup
	worker.Add(1)
	t.Cleanup(func() { cancel(); worker.Wait() })
	go func() {
		defer worker.Done()
		nodes, more, err := nameOnlySymbolCandidates(ctx, store, "Target", 1, query.QueryOptions{})
		done <- answer{nodes, more, err}
	}()
	select {
	case observed := <-store.entered:
		deadline, ok := observed.Deadline()
		require.True(t, ok)
		require.Equal(t, original, deadline)
		require.Same(t, ctx, observed, "the delegate must receive the original context")
	case <-time.After(3 * time.Second):
		t.Fatal("coherent delegate did not enter")
	}
	cancel()
	select {
	case result := <-done:
		require.ErrorIs(t, result.err, context.Canceled)
		require.Empty(t, result.nodes)
		require.False(t, result.more)
	case <-time.After(3 * time.Second):
		t.Fatal("coherent delegate did not join")
	}
	_, _, err = nameOnlySymbolCandidates(ctx, store, "Target", 1, query.QueryOptions{})
	require.ErrorIs(t, err, context.Canceled)
	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer expire()
	_, _, err = nameOnlySymbolCandidates(expired, store, "Target", 1, query.QueryOptions{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.EqualValues(t, 1, store.calls.Load(), "ended contexts must not start a new coherent read")
}

type nameCandidateUnsupportedReader struct{ graph.Reader }

func TestNameOnlyUnsupportedReaderCannotClaimEmpty(t *testing.T) {
	store := graph.New()
	store.AddNode(&graph.Node{ID: "main.go::Target", Name: "Target", Kind: graph.KindFunction, FilePath: "main.go"})
	nodes, more, err := nameOnlySymbolCandidates(t.Context(), nameCandidateUnsupportedReader{store}, "Target", 1, query.QueryOptions{})
	require.ErrorIs(t, err, projectionSuperseded, "a reader without coherent selection/hydration cannot earn complete absence")
	require.Empty(t, nodes)
	require.False(t, more)
}

func TestNameOnlyClosedStoreCannotClaimEmptyResult(t *testing.T) {
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "name-closed.sqlite"))
	require.NoError(t, err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			require.NoError(t, sql.Close())
		}
	})
	sql.AddNode(&graph.Node{ID: "main.go::Target", Name: "Target", Kind: graph.KindFunction, FilePath: "main.go"})
	require.NoError(t, sql.Close())
	closed = true
	nodes, more, err := nameOnlySymbolCandidates(t.Context(), sql, "Target", 1, query.QueryOptions{})
	require.Error(t, err, "a closed database must not be reclassified as an exact empty search")
	require.Empty(t, nodes)
	require.False(t, more)
	require.NoError(t, t.Context().Err(), "storage failure must remain distinct from caller cancellation")
}

func TestNameOnlySearchKeepsOptionalContractExclusionBeforeHorizon(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "name-optional-contract.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	const wanted = "z.go::Needle"
	store.AddNode(&graph.Node{ID: wanted, Name: "Needle", Kind: graph.KindFunction, FilePath: "z.go", StartLine: 1})
	for i := range 9 {
		kind := []graph.NodeKind{graph.KindContract, graph.KindContractBridge, graph.KindConfigKey}[i%3]
		store.AddNode(&graph.Node{ID: fmt.Sprintf("a.go::Excluded%02d", i), Name: "Needle", Kind: kind, FilePath: "a.go", StartLine: i + 1})
	}
	engine := query.NewEngine(store)
	engine.SetRerank(nil)
	server := NewServer(engine, store, nil, nil, zap.NewNop(), nil)
	installForbiddenContractRuntime(t, server)
	for _, text := range []string{"Needle", "name:Needle"} {
		req := mcplib.CallToolRequest{}
		req.Params.Name = "search_symbols"
		req.Params.Arguments = map[string]any{"query": text, "query_class": "symbol", "expand": "off", "limit": 1, "max_per_file": 0, "max_bytes": 0}
		ctx, refusal := server.prepareContractConsumer(t.Context(), req, requestFreshness{}, capabilityRequest{})
		require.Nil(t, refusal)
		status := contractConsumerStatusFromContext(ctx)
		require.NotNil(t, status)
		require.Equal(t, contractConsumerOptional, status.mode)
		binding := contractAnalysisFromContext(ctx)
		require.NotNil(t, binding)
		result, callErr := server.handleSearchSymbols(ctx, req)
		binding.close()
		require.NoError(t, callErr)
		require.False(t, result.IsError, resultText(result))
		body := extractTextResult(t, result)
		rows, ok := body["results"].([]any)
		require.True(t, ok)
		require.Len(t, rows, 1)
		row := rows[0].(map[string]any)
		require.Equal(t, wanted, row["id"], "optional contract rows must not consume the core name horizon: %s", text)
		require.Equal(t, string(graph.KindFunction), row["kind"])
		require.Equal(t, false, body["truncated"])
		require.Empty(t, body["next_cursor"])
		require.False(t, status.consulted, "ordinary core name search must not activate a contract reader")
	}
}
