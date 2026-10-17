package indexer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type contractFileNodesCore struct {
	graph.Reader
	calls int
	guard *contractReadGuard
	t     *testing.T
	err   error
}

func (r *contractFileNodesCore) Unwrap() graph.Reader { return r.Reader }
func (r *contractFileNodesCore) SemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	if reader, ok := r.Reader.(graph.SemanticBindingTypeReader); ok {
		return reader.SemanticBindingTypes(sites)
	}
	return nil, nil
}
func (r *contractFileNodesCore) LayerContractFileNodesContext(ctx context.Context, repo string, paths []string) (map[string][]*graph.Node, error) {
	r.calls++
	require.True(r.t, r.guard.recorded(), "selected node reads stay inside their ledger bracket")
	rows, err := graph.ContractFileNodesContext(ctx, r.Reader, repo, paths)
	if r.err != nil {
		return rows, r.err
	}
	return rows, err
}

func fileNodesEvidence(t *testing.T, nodes []*graph.Node, edges []*graph.Edge) (*contractFollowupEvidence, *contractFileNodesCore, *int) {
	t.Helper()
	core := graph.New()
	core.AddBatch(nodes, edges)
	guard := &contractReadGuard{open: true}
	counted := &contractFileNodesCore{Reader: core, guard: guard, t: t}
	scratch := newFTSStore(t)
	e := &contractFollowupEvidence{Store: scratch, scratch: scratch, core: counted, ctx: t.Context(), guard: guard, ledger: newContractUnitLedger(), files: map[string]ContractFollowupFile{"repo/handler.go": {Path: "repo/handler.go", RepoPrefix: "repo"}}, allowedRepos: map[string]bool{"repo": true}}
	fullCalls := new(int)
	e.readCore = func(context.Context, ContractFollowupFile) (ContractFollowupCoreFile, error) {
		require.True(t, guard.recorded())
		*fullCalls++
		return ContractFollowupCoreFile{Nodes: nodes, Edges: edges}, nil
	}
	return e, counted, fullCalls
}
func fileNodeInputs() []*graph.Node {
	return []*graph.Node{
		{ID: "repo/handler.go", Kind: graph.KindFile, FilePath: "repo/handler.go", RepoPrefix: "repo"},
		{ID: "repo/handler.go::Handler", Name: "Handler", Kind: graph.KindFunction, FilePath: "repo/handler.go", RepoPrefix: "repo", Meta: map[string]any{"signature": "func Handler()", "nested": map[string]any{"value": "core"}}},
	}
}

func TestContractFileNodesEvidenceReplayIgnoresOnlyUnconsumedEdges(t *testing.T) {
	nodes := fileNodeInputs()
	edge := &graph.Edge{From: nodes[1].ID, To: "repo/other.go::Other", Kind: graph.EdgeCalls, FilePath: "repo/other.go"}
	e, core, full := fileNodesEvidence(t, nodes, []*graph.Edge{edge})
	found := e.GetFileNodesByPaths([]string{"repo/handler.go"})
	require.NoError(t, e.err)
	require.Len(t, found["repo/handler.go"], 2)
	require.Equal(t, 1, core.calls)
	require.Zero(t, *full, "node-only consumers never import outgoing edges")
	require.Empty(t, e.GetOutEdges(nodes[1].ID))
	read := e.ledger.reads[0]
	require.Equal(t, contractReadFileNodes, read.kind)
	require.Contains(t, read.owners, "repo/handler.go")
	require.NotContains(t, read.owners, "repo/other.go")
	for _, mode := range []string{"ordinary_edge", "node_metadata", "removed_node"} {
		t.Run(mode, func(t *testing.T) {
			current := fileNodeInputs()
			if mode == "node_metadata" {
				current[1].Meta["signature"] = "func Handler(value int)"
			}
			if mode == "removed_node" {
				current = current[:1]
			}
			next, _, _ := fileNodesEvidence(t, current, []*graph.Edge{{From: nodes[1].ID, To: "repo/new.go::New", Kind: graph.EdgeArgOf, FilePath: "repo/new.go"}})
			equal, err := (&contractConvergence{}).replay(next, []contractRecordedRead{read}, nil)
			require.NoError(t, err)
			require.Equal(t, mode == "ordinary_edge", equal)
		})
	}
	for _, path := range []string{"repo/handler.go", "repo/other.go"} {
		moves := contractMoves{moved: map[string]bool{path: true}, declared: map[string]bool{"path:" + path: true}, byPath: map[string][]string{path: {"path:" + path}}, declaredKnown: true}
		require.Equal(t, path == "repo/handler.go", moves.affected(read))
		require.Equal(t, path == "repo/handler.go", moves.depends(path, read), "debt dependencies follow the node query's file, not unrelated edge ownership")
	}
}

func TestContractFileNodesThenFullLoadPreservesScratchWritesAndImportsEdges(t *testing.T) {
	nodes := fileNodeInputs()
	edge := &graph.Edge{From: nodes[1].ID, To: "repo/other.go::Other", Kind: graph.EdgeCalls, FilePath: "repo/other.go", Meta: map[string]any{"value": "core-edge"}}
	e, core, full := fileNodesEvidence(t, nodes, []*graph.Edge{edge})
	first := e.GetFileNodesByPaths([]string{"repo/handler.go"})["repo/handler.go"]
	require.NoError(t, e.err)
	handler := slices.IndexFunc(first, func(n *graph.Node) bool { return n.ID == nodes[1].ID })
	require.NotEqual(t, -1, handler)
	first[handler].Meta["nested"].(map[string]any)["value"] = "enriched"
	e.AddNode(first[handler])
	derived := &graph.Node{ID: "repo/handler.go::Derived", Kind: graph.KindType, FilePath: "repo/handler.go", RepoPrefix: "repo", Meta: map[string]any{"shape": "derived"}}
	e.AddNode(derived)
	loaded, edges, err := e.loadFile("repo/handler.go")
	require.NoError(t, err)
	require.Equal(t, 1, *full, "later full consumers still read complete checked edge evidence")
	require.Len(t, loaded, 3)
	got := e.GetNode(nodes[1].ID)
	require.Equal(t, "enriched", got.Meta["nested"].(map[string]any)["value"])
	require.Equal(t, "core", nodes[1].Meta["nested"].(map[string]any)["value"], "selected rows remain detached")
	require.Equal(t, derived, e.GetNode(derived.ID))
	require.Equal(t, []*graph.Edge{edge}, edges)
	require.Equal(t, []*graph.Edge{edge}, e.GetOutEdges(nodes[1].ID))
	_, again, err := e.loadFile("repo/handler.go")
	require.NoError(t, err)
	require.Equal(t, edges, again)
	require.Equal(t, 1, *full)
	require.Equal(t, 1, core.calls)
	fullRead := slices.IndexFunc(e.ledger.reads, func(read contractRecordedRead) bool {
		return read.kind == contractReadFile && slices.Equal(read.args, []string{"repo/handler.go"})
	})
	require.NotEqual(t, -1, fullRead, "the checked full-file answer is recorded")
	require.Contains(t, e.ledger.reads[fullRead].owners, "repo/other.go")
}

func TestContractFileNodesEvidenceRejectsUncertifiableAnswers(t *testing.T) {
	for _, mode := range []string{"missing_owner", "wrong_repo", "partial_error", "excluded", "outside_census"} {
		t.Run(mode, func(t *testing.T) {
			nodes := fileNodeInputs()
			if mode == "missing_owner" {
				nodes = nodes[1:]
			}
			if mode == "wrong_repo" {
				nodes[1].RepoPrefix = "outside"
			}
			e, core, _ := fileNodesEvidence(t, nodes, nil)
			path := "repo/handler.go"
			if mode == "partial_error" {
				core.err = errors.New("checked selected nodes failed")
			}
			if mode == "excluded" {
				path = "repo/excluded.go"
				e.excluded = map[string]bool{path: true}
			}
			if mode == "outside_census" {
				path = "repo/outside.go"
			}
			got := e.GetFileNodesByPaths([]string{path})
			if mode == "excluded" {
				require.NoError(t, e.err)
				require.Empty(t, got[path])
				require.Equal(t, contractReadFileNodes, e.ledger.reads[0].kind)
				require.Contains(t, e.ledger.reads[0].owners, path)
			} else {
				require.Error(t, e.err)
				require.Nil(t, got)
				require.Empty(t, e.loadedFileNodes)
				require.Empty(t, e.ledger.reads)
			}
		})
	}
}

func TestContractFileNodesActualCaptureWorkerMatchesIndependentCold(t *testing.T) {
	files := map[string]string{
		"routes.go":  "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /before\", handle) }\n",
		"handler.go": "package fixture\nimport \"net/http\"\nfunc handle(w http.ResponseWriter, r *http.Request) { writeJSON(w,200,Reply{Name:\"ok\"}) }\nfunc writeJSON(w http.ResponseWriter, code int, body any) {}\n",
		"types.go":   "package fixture\ntype Reply struct { Name string `json:\"name\"` }\n",
	}
	f := newContractJobFixture(t, files)
	calls := 0
	f.adjust = func(snapshot *ContractFollowupSnapshot) {
		require.IsType(t, &contractCapturedCore{}, snapshot.Core, "exercise the production selected-cohort dispatcher")
		counted := &contractFileNodesCore{Reader: snapshot.Core, t: t}
		snapshot.Core = counted
		release := snapshot.Release
		snapshot.Release = func() { calls += counted.calls; release() }
	}
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	require.Positive(t, calls, "the real handler postpass must exercise checked node-only reads")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	previous := calls
	files["routes.go"] = strings.ReplaceAll(files["routes.go"], "/before", "/after")
	f.save("routes.go", files["routes.go"])
	runCachedContractFixture(t, f, &cache)
	require.Greater(t, calls, previous, "delta postpass must also use newly captured node evidence")
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
}

func TestContractFileNodesCapturedDispatcherNeverSubstitutesPrimary(t *testing.T) {
	primary := graph.New()
	primary.AddBatch(fileNodeInputs(), nil)
	selected := graph.New()
	core := &contractCapturedCore{Reader: primary, readers: map[string]graph.Reader{"repo": selected}}
	rows, err := graph.ContractFileNodesContext(t.Context(), core, "repo", []string{"repo/handler.go"})
	require.NoError(t, err)
	require.Empty(t, rows["repo/handler.go"], "a primary owner cannot answer the selected generation's negative")
	current := fileNodeInputs()
	current[1].Meta["signature"] = "func Handler(selected int)"
	selected.AddBatch(current, nil)
	rows, err = graph.ContractFileNodesContext(t.Context(), core, "repo", []string{"repo/handler.go"})
	require.NoError(t, err)
	require.ElementsMatch(t, current, rows["repo/handler.go"])
	rows, err = graph.ContractFileNodesContext(t.Context(), core, "outside", []string{"repo/handler.go"})
	require.ErrorIs(t, err, graph.ErrContractProjectionUnsupported)
	require.Nil(t, rows)
}

func TestContractFollowupCrossFileHandlerResolvesSiblingResponseShape(t *testing.T) {
	files := map[string]string{
		"routes.go":  "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /before\", handle) }\n",
		"handler.go": "package fixture\nimport \"net/http\"\nfunc handle(w http.ResponseWriter, r *http.Request) { response := Reply{Name:\"ok\"}; WriteJSON(w,200,response) }\nfunc WriteJSON(w http.ResponseWriter, code int, body any) {}\n",
		"types.go":   "package fixture\ntype Reply struct { Name string `json:\"name\"` }\n",
	}
	f := newContractJobFixture(t, files)
	assertShape := func(route string) {
		t.Helper()
		inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
		require.NoError(t, err)
		analysis, err := f.options.Materializer.OpenContractAnalysisForInputs(t.Context(), nil, inputs)
		require.NoError(t, err)
		defer analysis.Close()
		nodes, err := analysis.NodesByKindsContext(t.Context(), []graph.NodeKind{graph.KindType})
		require.NoError(t, err)
		require.Len(t, nodes, 1)
		require.Equal(t, "fixture/types.go::Reply", nodes[0].ID)
		require.NotNil(t, nodes[0].Meta["shape"], "the actual independently leased payload must carry the sibling response shape")
		contracts := f.contracts()
		require.Contains(t, contracts, "http::GET::"+route)
		meta, ok := contracts["http::GET::"+route].Meta["contract_meta"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "fixture/types.go::Reply", meta["response_type"])
		require.NoError(t, analysis.Validate(t.Context()))
	}
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	assertShape("/before")
	files["routes.go"] = strings.ReplaceAll(files["routes.go"], "/before", "/after")
	f.save("routes.go", files["routes.go"])
	runCachedContractFixture(t, f, &cache)
	assertShape("/after")
	require.NotContains(t, f.contracts(), "http::GET::/before")
}
