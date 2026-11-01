package mcp

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// Over a store holding six repositories, a session bound to two of them gets
// the same callers and call chain whether the request reader exposes the
// store's optional capabilities or only the plain Store contract: the scoped
// admission statement and the repository-narrowed placeholder read are pure
// accelerations. Boundaries, lower bounds and truncation ride the compared
// payloads. Dynamic boundaries do not: they are detected in the target's
// source on disk, and this fixture writes none, so they stay empty in every
// arm.
//
// The contract-core wrapper hides matches edges, so only the unwrapped arm
// may show the cross-repository match.
func TestGetCallersAndCallChainScopedGoldenMultiRepo(t *testing.T) {
	store := openContractCoreCapStore(t)
	addScopedRowRepo(t, store, "a")
	addScopedRowRepo(t, store, "b")
	addUnrelatedScopedRowRepos(t, store, 4)
	handler, target := scopedRowID("a", scopedRowHandler), scopedRowID("b", scopedRowTarget)
	require.NoError(t, store.AddBatchChecked(nil, []*graph.Edge{
		{From: handler, To: target, Kind: graph.EdgeMatches, FilePath: "a/svc/handler.go", Line: 11, Origin: graph.OriginASTResolved, Confidence: .9},
	}))
	server := func(backend graph.Store, wrapped bool) *Server {
		srv := NewServer(query.NewEngine(backend), backend, nil, nil, zap.NewNop(), nil)
		if wrapped {
			installContractCoreKindTestRuntime(t, srv)
		}
		return srv
	}
	requests := []struct {
		tool string
		args map[string]any
	}{
		{"get_callers", map[string]any{"id": target, "format": "json"}},
		{"get_callers", map[string]any{"id": target, "format": "json", "min_tier": graph.OriginTextMatched}},
		{"get_callers", map[string]any{"id": target, "format": "json", "depth": 1, "limit": 2}},
		{"get_call_chain", map[string]any{"id": handler, "format": "json"}},
		{"get_call_chain", map[string]any{"id": handler, "format": "json", "depth": 2, "limit": 3}},
	}
	sessions := 0
	payloads := func(srv *Server) []string {
		out := make([]string, 0, len(requests))
		for _, request := range requests {
			sessions++
			ctx := outlineBoundContext(srv, fmt.Sprintf("golden-%d", sessions), "", map[string]bool{"a": true, "b": true})
			handle := srv.handleGetCallers
			if request.tool == "get_call_chain" {
				handle = srv.handleGetCallChain
			}
			result, err := srv.wrapToolHandler(handle)(ctx, makeReq(request.tool, request.args))
			require.NoError(t, err)
			require.False(t, result.IsError, toolResultText(result))
			out = append(out, toolResultText(result))
		}
		return out
	}
	production := payloads(server(store, true))
	require.Equal(t, payloads(server(struct{ graph.Store }{store}, true)), production, "optional capabilities changed a scoped payload")
	unwrapped := payloads(server(store, false))
	require.Equal(t, payloads(server(struct{ graph.Store }{store}, false)), unwrapped, "optional capabilities changed a scoped payload containing cross-repository matches")

	var callers struct {
		Nodes []struct {
			ID         string `json:"id"`
			RepoPrefix string `json:"repo_prefix"`
		} `json:"nodes"`
		Boundaries         []json.RawMessage `json:"boundaries"`
		LowerBound         bool              `json:"lower_bound"`
		NameOnlyCandidates int               `json:"name_only_candidates"`
	}
	require.NoError(t, json.Unmarshal([]byte(production[0]), &callers))
	require.NotEmpty(t, callers.Boundaries, "the interface method the target overrides is a dispatch boundary")
	require.True(t, callers.LowerBound)
	require.Equal(t, 5, callers.NameOnlyCandidates, "two bare call sites in a and three in b, none from the other four repositories")
	for _, node := range callers.Nodes {
		require.Contains(t, []string{"a", "b"}, node.RepoPrefix, node.ID)
	}
	for i, payload := range production {
		require.NotContains(t, payload, `"kind":"matches"`, "request %d", i)
	}
	require.Contains(t, unwrapped[0], `"kind":"matches"`, "the unwrapped caller walk crosses the match")
	require.Contains(t, unwrapped[3], `"kind":"matches"`, "the unwrapped call chain crosses the match")
	var unwrappedCallers struct {
		Boundaries []json.RawMessage `json:"boundaries"`
	}
	require.NoError(t, json.Unmarshal([]byte(unwrapped[0]), &unwrappedCallers))
	require.NotEmpty(t, unwrappedCallers.Boundaries, "the unwrapped oracle comparison includes the interface dispatch boundary")
}
