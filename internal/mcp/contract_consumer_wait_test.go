package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/runtimeactivity"
	"github.com/zzet/gortex/internal/search"
)

type contractConsumerFixture struct {
	srv      *Server
	store    *store_sqlite.Store
	root     string
	states   map[string]graph.ContractInputState
	sequence int
}

func newContractConsumerFixture(t *testing.T) *contractConsumerFixture {
	t.Helper()
	root := t.TempDir()
	store, err := store_sqlite.Open(filepath.Join(root, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	f := &contractConsumerFixture{store: store, root: root, states: make(map[string]graph.ContractInputState)}
	for _, repo := range []string{"repo", "b"} {
		path := filepath.Join(root, repo, "handler.go")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("package fixture\nfunc Handle() {}\n"), 0o644))
		file := repo + "/handler.go"
		store.AddBatch([]*graph.Node{
			{ID: file, Name: "handler.go", Kind: graph.KindFile, FilePath: file, RepoPrefix: repo},
			{ID: file + "::Handle", Name: "Handle", Kind: graph.KindFunction, FilePath: file, RepoPrefix: repo, WorkspaceID: "ws", ProjectID: "project", StartLine: 2, EndLine: 2},
		}, nil)
		state := graph.ContractInputState{RepoPrefix: repo, InputVersion: "boundary-v1", InputFingerprint: repo + "-new", Accepted: true}
		require.NoError(t, store.BeginContractInputMutationContext(t.Context(), nil, state, nil))
		require.NoError(t, store.AcceptContractInputMutationContext(t.Context(), state))
		f.states[repo] = state
	}
	store.AddBatch([]*graph.Node{{ID: "repo/caller.go::InvokeCaller", Name: "InvokeCaller", Kind: graph.KindFunction, FilePath: "repo/caller.go", RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "project"}}, []*graph.Edge{{From: "repo/caller.go::InvokeCaller", To: "repo/handler.go::Handle", Kind: graph.EdgeCalls, FilePath: "repo/caller.go"}})
	// Legacy core contract data is deliberately stale. Optional searches must
	// exclude it, and explicit consumers must read the isolated replacement.
	store.AddNode(&graph.Node{ID: "http::GET::/before", Name: "Handle stale route", Kind: graph.KindContract, RepoPrefix: "repo", FilePath: "repo/handler.go", Meta: map[string]any{"type": "http", "role": "provider", "contract_meta": map[string]any{"path": "/before", "method": "GET"}}})
	engine := query.NewEngine(store)
	engine.SetSearch(search.NewSymbolSearcherBackend(store))
	f.srv = NewServer(engine, store, nil, nil, zap.NewNop(), nil)
	f.srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: graphview.NewLeaseManager()})
	return f
}

func (f *contractConsumerFixture) request(ctx context.Context, name string, args map[string]any, handler mcpserver.ToolHandlerFunc) (*mcplib.CallToolResult, error) {
	req := mcplib.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	return f.srv.wrapToolHandler(handler)(ctx, req)
}

func (f *contractConsumerFixture) publish(t *testing.T, selected *graphview.RepoView, actor string, route string) {
	f.publishRows(t, selected, actor, route, nil, nil)
}

func (f *contractConsumerFixture) publishRows(t *testing.T, selected *graphview.RepoView, actor, route string, nodes []*graph.Node, edges []*graph.Edge) {
	t.Helper()
	var captures []*graphview.SelectedContractInputs
	for _, repo := range []string{"repo", "b"} {
		var input *graphview.SelectedContractInputs
		var err error
		if selected != nil && selected.ID.RepoPrefix != repo {
			input, err = f.srv.materializer.CaptureContractCompanionInputs(t.Context(), selected, repo, actor)
		} else {
			input, err = f.srv.materializer.CaptureContractInputs(t.Context(), selected, repo, actor)
		}
		require.NoError(t, err)
		captures = append(captures, input)
	}
	id := "http::GET::" + route
	meta := map[string]any{"path": route, "method": "GET", "framework": "test-framework", "response_type": "repo/types.go::Response", "response_envelope": []map[string]any{{"name": "fresh"}}}
	canonical := &graph.Node{ID: id, Name: "GET " + route, Kind: graph.KindContract, FilePath: "repo/handler.go", RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "project", Meta: map[string]any{"type": "http", "role": "provider", "symbol_id": "repo/handler.go::Handle", "workspace": "ws", "project": "project", "contract_owner_record": true, "contract_meta": meta}}
	bridge := &graph.Node{ID: "bridge::ws::project::" + id, Name: "GET " + route, Kind: graph.KindContractBridge, FilePath: indexer.ContractBridgeFilePath, RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "project", Meta: map[string]any{"contract_type": "http", "canonical_key": "GET " + route, "contract_id": id, "workspace": "ws", "project": "project", "repos": []string{"repo", "b"}, "provider_count": 1, "consumer_count": 1, "cross_repo": true}}
	for _, repo := range []string{"repo", "b"} {
		input, err := graphview.ComposeSelectedContractInputs(repo, actor, captures...)
		require.NoError(t, err)
		f.sequence++
		generation, handle, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "ref_view", GraphID: "contract-test", LayerID: fmt.Sprintf("analysis-%d", f.sequence), GenerationKind: "commit", TreeOID: fmt.Sprintf("analysis-tree-%d", f.sequence), CreatedAt: 1})
		require.NoError(t, err)
		ownerMeta := make(map[string]any, len(meta))
		for key, value := range meta {
			ownerMeta[key] = value
		}
		kind := graph.EdgeProvides
		if repo == "b" {
			kind = graph.EdgeConsumes
			ownerMeta["response_type"] = "b/types.go::Response"
		}
		owner := &graph.Edge{From: repo + "/handler.go::Handle", To: id, Kind: kind, FilePath: repo + "/handler.go", Meta: map[string]any{"contract_owner_repo_prefix": repo, "contract_owner_workspace": "ws", "contract_owner_project": "project", "contract_owner_type": "http", "contract_owner_symbol_id": repo + "/handler.go::Handle", "contract_owner_meta": ownerMeta}}
		field := "fresh"
		if repo == "b" {
			field = "missing"
		}
		shape := &graph.Node{ID: repo + "/types.go::Response", Name: "Response", Kind: graph.KindType, FilePath: repo + "/types.go", RepoPrefix: repo, Meta: map[string]any{"shape": &contracts.Shape{Kind: "struct", Fields: []contracts.ShapeField{{Name: field, Type: "string"}}}}}
		handle.AddBatch(append([]*graph.Node{canonical, bridge, shape}, nodes...), append([]*graph.Edge{owner, {From: bridge.ID, To: id, Kind: graph.EdgeBridges, FilePath: indexer.ContractBridgeFilePath, Meta: map[string]any{"side": "both"}}}, edges...))
		require.NoError(t, handle.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{
			{NodeID: canonical.ID, Tokens: strings.Join(search.Tokenize(canonical.Name), " ")},
			{NodeID: bridge.ID, Tokens: strings.Join(search.Tokenize(bridge.Name), " ")},
		}))
		require.NoError(t, handle.SetProducerState(store_sqlite.ProducerCompleteness{Producer: string(graphview.CapContracts), State: store_sqlite.ProducerStateComplete}))
		attachment := graph.ContractAttachment{RepoPrefix: repo, CheckoutID: actor, PayloadGeneration: generation, InputVersion: input.State.InputVersion, InputFingerprint: input.State.InputFingerprint}
		receiver := f.store
		if selected != nil {
			receiver = f.store.AtGeneration(selected.GenerationSources()[len(selected.GenerationSources())-1].Handle.ViewGeneration())
		}
		require.NoError(t, receiver.PublishContractAttachmentWithInputsContext(t.Context(), input.State, input.Witnesses, attachment, nil, 2))
	}
}

func requireContractBody(t *testing.T, result *mcplib.CallToolResult, err error) string {
	t.Helper()
	require.NoError(t, err)
	require.NotNil(t, result)
	text := viewResultText(t, result)
	require.False(t, result.IsError, text)
	return text
}

func TestContractConsumerHeldWorkerKeepsOrdinaryHandlersIndependent(t *testing.T) {
	f := newContractConsumerFixture(t)
	idx := indexer.New(graph.New(), testRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("repo")
	_, err := idx.Index(filepath.Join(f.root, "repo"))
	require.NoError(t, err)
	f.srv.indexer = idx
	held := make(chan struct{})
	queued := make(chan struct{}, 1)
	var requests, waits atomic.Int64
	f.srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{
		Request: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error) {
			if f.srv.ContractAnalysisShouldYield() {
				t.Error("waiting contract RPC must not park its own producer")
			}
			requests.Add(1)
			return true, nil
		},
		WaitChange: func(ctx context.Context, _ graph.ContractAttachmentKey) error {
			waits.Add(1)
			select {
			case queued <- struct{}{}:
			default:
			}
			select {
			case <-held:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	type answer struct {
		result *mcplib.CallToolResult
		err    error
	}
	done := make(chan answer, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	f.srv.NoteSessionClient("contract-held-facade", "codex", "1")
	facadeCtx := WithSessionID(ctx, "contract-held-facade")
	go func() {
		result, err := f.request(facadeCtx, "analyze", map[string]any{"kind": "contracts", "options": map[string]any{"action": "list", "wait_deadline": time.Now().Add(8 * time.Second).Format(time.RFC3339Nano)}, "output": map[string]any{"format": "json"}}, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			return f.srv.handleFacade(ctx, "analyze", req)
		})
		done <- answer{result, err}
	}()
	select {
	case <-queued:
	case <-ctx.Done():
		t.Fatal("strict contract RPC did not enter the worker wait")
	}
	select {
	case result := <-done:
		t.Fatalf("contract RPC returned before worker release: %+v", result)
	default:
	}
	for _, test := range []struct {
		name     string
		args     map[string]any
		handler  mcpserver.ToolHandlerFunc
		contains string
	}{
		{"read_file", map[string]any{"path": filepath.Join(f.root, "repo", "handler.go")}, f.srv.handleReadFile, "func Handle"},
		{"get_symbol", map[string]any{"id": "repo/handler.go::Handle", "format": "json"}, f.srv.handleGetSymbol, "Handle"},
		{"get_callers", map[string]any{"id": "repo/handler.go::Handle", "format": "json"}, f.srv.handleGetCallers, "InvokeCaller"},
		{"search_text", map[string]any{"query": "func Handle", "format": "json"}, f.srv.handleSearchText, "func Handle"},
		{"search_symbols", map[string]any{"query": "Handle", "format": "json"}, f.srv.handleSearchSymbols, "Handle"},
	} {
		result, err := f.request(ctx, test.name, test.args, test.handler)
		body := requireContractBody(t, result, err)
		require.Contains(t, body, test.contains)
		if test.name == "search_symbols" {
			require.NotContains(t, body, "/before")
			require.Contains(t, body, "contract_component_not_loaded")
		}
		require.EqualValues(t, 1, requests.Load(), "ordinary %s queued contract analysis", test.name)
		require.EqualValues(t, 1, waits.Load(), "ordinary %s waited for contract analysis", test.name)
	}
	f.publish(t, nil, "", "/after")
	close(held)
	select {
	case result := <-done:
		body := requireContractBody(t, result.result, result.err)
		require.Contains(t, body, "/after")
		require.NotContains(t, body, "/before")
		require.Contains(t, body, `"state":"complete"`)
	case <-ctx.Done():
		t.Fatal("same contract RPC did not complete after publication")
	}
}

func TestContractConsumerOrdinaryNameSearchDoesNotRequireAnalysisStorage(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.srv.SetMaterializer(nil)
	f.srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{
		Request: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error) {
			t.Error("ordinary name search scheduled contract work")
			return false, nil
		},
		WaitChange: func(context.Context, graph.ContractAttachmentKey) error {
			t.Error("ordinary name search waited for contract work")
			return nil
		},
	})
	result, err := f.request(t.Context(), "search_symbols", map[string]any{"query": "Handle", "format": "json"}, f.srv.handleSearchSymbols)
	body := requireContractBody(t, result, err)
	require.Contains(t, body, "repo/handler.go::Handle")
	require.NotContains(t, body, "/before")
	require.Contains(t, body, "contract_component_not_loaded")
}

func TestContractConsumerRequiredMixedSearchAndRelationsPreserveCore(t *testing.T) {
	f := newContractConsumerFixture(t)
	core := &graph.Node{ID: "repo/handler.go::AfterHelper", Name: "AfterHelper", Kind: graph.KindFunction, FilePath: "repo/handler.go", RepoPrefix: "repo"}
	table := &graph.Node{ID: "repo/schema.sql::items", Name: "items", Kind: graph.KindType, FilePath: "repo/schema.sql", RepoPrefix: "repo"}
	f.store.AddBatch([]*graph.Node{core, table}, []*graph.Edge{{From: "repo/handler.go::Handle", To: table.ID, Kind: graph.EdgeProvides, FilePath: "repo/schema.sql"}})
	require.NoError(t, f.store.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: core.ID, Tokens: "after helper"}}))
	f.publish(t, nil, "", "/after")
	f.srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{
		Request: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error) {
			t.Error("current mixed request scheduled contract work")
			return false, nil
		},
		WaitChange: func(context.Context, graph.ContractAttachmentKey) error {
			t.Error("current mixed request waited")
			return nil
		},
	})
	result, err := f.request(t.Context(), "search_symbols", map[string]any{"query": "after", "format": "json", requiredCapabilitiesArgName: string(graphview.CapContracts)}, f.srv.handleSearchSymbols)
	body := requireContractBody(t, result, err)
	require.Contains(t, body, core.ID)
	require.Contains(t, body, "http::GET::/after")
	require.NotContains(t, body, "/before")
	result, err = f.request(t.Context(), "graph_query", map[string]any{"query": "nodes name~Handle | traverse provides,calls both", "format": "json", requiredCapabilitiesArgName: string(graphview.CapContracts)}, f.srv.handleGraphQuery)
	body = requireContractBody(t, result, err)
	require.Contains(t, body, table.ID, "noncontract SQL Provides edge must survive")
	require.Contains(t, body, "InvokeCaller", "ordinary caller edge must survive")
	require.Contains(t, body, "http::GET::/after")
	require.NotContains(t, body, "/before")
}

func TestContractConsumerWaitHonorsOriginalDeadlineAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			f := newContractConsumerFixture(t)
			entered := make(chan struct{}, 1)
			f.srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{Request: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error) {
				return true, nil
			}, WaitChange: func(ctx context.Context, _ graph.ContractAttachmentKey) error {
				entered <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			args := map[string]any{"action": "list", "wait_deadline": time.Now().Add(time.Second).Format(time.RFC3339Nano)}
			type answer struct {
				result *mcplib.CallToolResult
				err    error
			}
			done := make(chan answer, 1)
			go func() {
				result, err := f.request(ctx, "contracts", args, f.srv.handleContracts)
				done <- answer{result, err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("contract wait did not start")
			}
			if canceled {
				cancel()
			}
			select {
			case result := <-done:
				if result.err != nil {
					require.True(t, canceled)
					require.ErrorIs(t, result.err, context.Canceled)
					require.Nil(t, result.result)
				} else {
					require.NotNil(t, result.result)
					require.True(t, result.result.IsError)
					require.Contains(t, viewResultText(t, result.result), "required_capability_incomplete")
					require.NotContains(t, viewResultText(t, result.result), "/before")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("contract wait ignored deadline/cancellation")
			}
		})
	}
}

func TestContractConsumerRecapturesSupersededInputsWithinSameRPC(t *testing.T) {
	f := newContractConsumerFixture(t)
	first := make(chan struct{})
	changed := make(chan struct{})
	current := make(chan *graphview.SelectedContractInputs, 1)
	held := make(chan struct{})
	var attempts atomic.Int64
	f.srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{
		Request: func(ctx context.Context, _ *graphview.RepoView, inputs *graphview.SelectedContractInputs, _, _ string) (bool, error) {
			if attempts.Add(1) == 1 {
				close(first)
				select {
				case <-changed:
					return false, graph.ErrContractProjectionStale
				case <-ctx.Done():
					return false, ctx.Err()
				}
			}
			current <- inputs
			return true, nil
		},
		WaitChange: func(ctx context.Context, _ graph.ContractAttachmentKey) error {
			select {
			case <-held:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan *mcplib.CallToolResult, 1)
	go func() {
		result, _ := f.request(ctx, "contracts", map[string]any{"action": "list", "format": "json", "wait_deadline": time.Now().Add(8 * time.Second).Format(time.RFC3339Nano)}, f.srv.handleContracts)
		done <- result
	}()
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("first selected input request did not start")
	}
	old := f.states["repo"]
	next := old
	next.InputFingerprint = "repo-newer"
	require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &old, next, nil))
	require.NoError(t, f.store.AcceptContractInputMutationContext(t.Context(), next))
	f.states["repo"] = next
	close(changed)
	select {
	case capture := <-current:
		found := false
		for _, witness := range capture.Witnesses {
			if witness.State.RepoPrefix == "repo" && witness.Found {
				require.Equal(t, next.InputFingerprint, witness.State.InputFingerprint)
				found = true
			}
		}
		require.True(t, found)
	case <-ctx.Done():
		t.Fatal("same RPC did not recapture superseded contract inputs")
	}
	f.publish(t, nil, "", "/after-newer-input")
	close(held)
	select {
	case result := <-done:
		require.False(t, result.IsError, viewResultText(t, result))
		require.Contains(t, viewResultText(t, result), "/after-newer-input")
	case <-ctx.Done():
		t.Fatal("same RPC did not return the current attachment")
	}
}

func TestContractConsumerNonemptySelectedHandlersUseOneCoherentCohort(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/after")
	f.srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{Request: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error) {
		t.Error("ready attachment queued work")
		return false, nil
	}, WaitChange: func(context.Context, graph.ContractAttachmentKey) error {
		t.Error("ready attachment waited")
		return nil
	}})
	for _, test := range []struct {
		name     string
		args     map[string]any
		handler  mcpserver.ToolHandlerFunc
		contains string
	}{
		{"contracts", map[string]any{"action": "check", "format": "json"}, f.srv.handleContracts, `"matched_pairs":1`},
		{"contracts", map[string]any{"action": "validate", "format": "json"}, f.srv.handleContracts, "response_field_removed"},
		{"api_impact", map[string]any{"route": "/after", "format": "json"}, f.srv.handleAPIImpact, `"success":["fresh"]`},
		{"contracts", map[string]any{"action": "bridge", "query": "after", "format": "json"}, f.srv.handleContracts, "bridge::ws::project::http::GET::/after"},
		{"contracts", map[string]any{"action": "bridge", "mode": "impact", "symbol": "repo/handler.go::Handle", "format": "json"}, f.srv.handleContracts, "bridge::ws::project::http::GET::/after"},
		{"analyze", map[string]any{"kind": "route_frameworks", "format": "json"}, f.srv.handleAnalyze, `"test-framework":1`},
		{"get_symbol", map[string]any{"id": "http::GET::/after", "format": "json"}, f.srv.handleGetSymbol, "http::GET::/after"},
		{"analyze", map[string]any{"kind": "contracts", "options": map[string]any{"action": "check"}, "output": map[string]any{"format": "json"}}, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			return f.srv.handleFacade(ctx, "analyze", req)
		}, `"matched_pairs":1`},
		{"change", map[string]any{"operation": "api_impact", "options": map[string]any{"route": "/after"}, "output": map[string]any{"format": "json"}}, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			return f.srv.handleFacade(ctx, "change", req)
		}, `"success":["fresh"]`},
	} {
		t.Run(test.name+"/"+fmt.Sprint(test.args), func(t *testing.T) {
			ctx := t.Context()
			if test.args["output"] != nil {
				f.srv.NoteSessionClient("contract-current-facade", "codex", "1")
				ctx = WithSessionID(ctx, "contract-current-facade")
			}
			result, err := f.request(ctx, test.name, test.args, test.handler)
			body := requireContractBody(t, result, err)
			require.Contains(t, body, test.contains)
			require.NotContains(t, body, "/before")
			var object map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &object))
			require.Equal(t, "complete", object["contract_analysis"].(map[string]any)["state"])
		})
	}
}

func TestContractConsumerLinkedCompanionBindingUsesActualMaterializedAncestry(t *testing.T) {
	f := newContractConsumerFixture(t)
	graphID := indexer.GraphIDFor("repo")
	worktreeRoot := filepath.Join(f.root, "worktree")
	require.NoError(t, os.MkdirAll(worktreeRoot, 0o755))
	seedViewCatalog(t, f.store, graphID, filepath.Join(f.root, "repo"), worktreeRoot)
	generation, handle, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: graphID, CheckoutID: viewTestWorktree, LayerID: "selected-contract-core", GenerationKind: "commit", TreeOID: "selected-contract-tree", CreatedAt: 1000})
	require.NoError(t, err)
	state := graph.ContractInputState{RepoPrefix: "repo", CheckoutID: viewTestWorktree, InputVersion: "boundary-v1", InputFingerprint: "selected-linked", Accepted: true}
	require.NoError(t, handle.SetContractInputStateWithWorkContext(t.Context(), nil, state, nil))
	handle.AddBatch([]*graph.Node{{ID: "repo/handler.go", Name: "handler.go", Kind: graph.KindFile, FilePath: "repo/handler.go", RepoPrefix: "repo"}, {ID: "repo/handler.go::Handle", Name: "SelectedLinkedHandler", Kind: graph.KindFunction, FilePath: "repo/handler.go", RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "project"}}, nil)
	require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "repo", FilePath: "repo/handler.go", Mode: store_sqlite.OwnershipReplace}}))
	require.NoError(t, f.store.PublishPayloadGeneration(t.Context(), generation, 2000))
	dirty, dirtyHandle, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: graphID, CheckoutID: viewTestWorktree, LayerID: "selected-contract-dirty", GenerationKind: "dirty", BaseGenerationID: generation, TreeOID: "selected-contract-tree", CreatedAt: 3000})
	require.NoError(t, err)
	require.NoError(t, dirtyHandle.SetContractInputStateWithWorkContext(t.Context(), &state, state, nil))
	require.NoError(t, f.store.PublishPayloadGeneration(t.Context(), dirty, 4000))
	routeViewCheckout(t, f.store, graphID, generation, dirty, store_sqlite.RouteActive)
	selected, err := f.srv.materializer.MaterializeCheckout(t.Context(), viewTestWorktree)
	require.NoError(t, err)
	defer selected.Close()
	require.True(t, selected.ComposesBaseCorpus())
	f.publish(t, nil, "", "/primary-only")
	f.publish(t, selected, viewTestWorktree, "/linked-only")
	f.srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{Request: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error) {
		t.Error("selected attachments were not bound")
		return false, nil
	}, WaitChange: func(context.Context, graph.ContractAttachmentKey) error {
		t.Error("selected attachments waited")
		return nil
	}})
	ctx := WithSessionCWD(WithSessionID(t.Context(), "contract-linked"), worktreeRoot)
	result, err := f.request(ctx, "contracts", map[string]any{"action": "check", "format": "json", "view": map[string]any{"kind": "worktree", "checkout_id": viewTestWorktree}}, f.srv.handleContracts)
	body := requireContractBody(t, result, err)
	require.Contains(t, body, "/linked-only")
	require.Contains(t, body, `"matched_pairs":1`)
	require.NotContains(t, body, "/primary-only")
	// The companion attachment is selected under the linked cohort actor, not
	// the companion's latest primary actor. Core source fallback also uses the
	// selected linked source for repo, preserving its current declaration.
	binding, pending, err := f.srv.openContractConsumerBinding(t.Context(), selected, viewTestWorktree, []string{"repo", "b"})
	require.NoError(t, err)
	require.Nil(t, pending)
	defer binding.close()
	require.Equal(t, viewTestWorktree, binding.views["b"].Attachment.CheckoutID)
	reader, err := binding.analysisReader(t.Context())
	require.NoError(t, err)
	require.Equal(t, "SelectedLinkedHandler", reader.GetNode("repo/handler.go::Handle").Name)
}

func TestContractConsumerAdmissionExcludesOtherWorkspaces(t *testing.T) {
	stack := newViewStack(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "contracts"
	req.Params.Arguments = map[string]any{"action": "check"}
	ctx := WithSessionCWD(WithSessionID(t.Context(), "contract-workspace"), stack.repoRoot)
	repos, err := stack.srv.contractConsumerRepos(ctx, req)
	require.NoError(t, err)
	require.Equal(t, []string{"repo"}, repos)
	// An explicitly unbound caller retains all-repo semantics, rather than
	// quietly receiving the first repo merely to make the census cheaper.
	repos, err = stack.srv.contractConsumerRepos(t.Context(), req)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"repo", "other"}, repos)
}

func TestContractConsumerIndependentDependencyAndConfigEdgesKeepCoreDeclarations(t *testing.T) {
	f := newContractConsumerFixture(t)
	ctor := &graph.Node{ID: "repo/beans.java::Ctor", Name: "CurrentCoreCtor", Kind: graph.KindFunction, FilePath: "repo/beans.java", RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "project"}
	bean := &graph.Node{ID: "repo/beans.java::Bean", Name: "CurrentBean", Kind: graph.KindFunction, FilePath: ctor.FilePath, RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "project"}
	oldBean := &graph.Node{ID: "repo/beans.java::OldBean", Name: "OldBean", Kind: graph.KindFunction, FilePath: ctor.FilePath, RepoPrefix: "repo"}
	removed := &graph.Node{ID: "config::REMOVED", Name: "REMOVED", Kind: graph.KindConfigKey, RepoPrefix: "repo"}
	current := &graph.Node{ID: "config::DATABASE_URL", Name: "DATABASE_URL", Kind: graph.KindConfigKey, RepoPrefix: "repo", WorkspaceID: "ws", ProjectID: "project"}
	f.store.AddBatch([]*graph.Node{ctor, bean, oldBean, removed}, []*graph.Edge{
		{From: ctor.ID, To: oldBean.ID, Kind: graph.EdgeCalls, FilePath: ctor.FilePath, Meta: map[string]any{"via": "spring.Bean"}},
		{From: ctor.ID, To: removed.ID, Kind: graph.EdgeReadsConfig, FilePath: ctor.FilePath},
	})
	analysisCtor := *ctor
	analysisCtor.Name = "AnalysisSnapshotCtor"
	f.publishRows(t, nil, "", "/after", []*graph.Node{&analysisCtor, bean, current}, []*graph.Edge{
		{From: ctor.ID, To: bean.ID, Kind: graph.EdgeCalls, FilePath: ctor.FilePath, Meta: map[string]any{"via": "spring.Bean", "confidence": 1.0}},
		{From: ctor.ID, To: current.ID, Kind: graph.EdgeReadsConfig, FilePath: ctor.FilePath, Meta: map[string]any{"source": "configuration"}},
	})
	f.srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{Request: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error) {
		t.Error("complete dependency payload scheduled work")
		return false, nil
	}, WaitChange: func(context.Context, graph.ContractAttachmentKey) error {
		t.Error("complete dependency payload waited")
		return nil
	}})
	result, err := f.request(t.Context(), "get_symbol", map[string]any{"id": ctor.ID, "format": "json", requiredCapabilitiesArgName: string(graphview.CapContracts)}, f.srv.handleGetSymbol)
	body := requireContractBody(t, result, err)
	require.Contains(t, body, "CurrentCoreCtor")
	require.NotContains(t, body, "AnalysisSnapshotCtor")
	for _, traversal := range []string{"calls", "reads_config"} {
		result, err := f.request(t.Context(), "graph_query", map[string]any{"query": "nodes name~CurrentCoreCtor | traverse " + traversal + " out", "format": "json", requiredCapabilitiesArgName: string(graphview.CapContracts)}, f.srv.handleGraphQuery)
		body := requireContractBody(t, result, err)
		require.NotContains(t, body, "AnalysisSnapshotCtor")
		require.NotContains(t, body, "OldBean")
		require.NotContains(t, body, "REMOVED")
		if traversal == "calls" {
			require.Contains(t, body, bean.ID)
		} else {
			require.Contains(t, body, current.ID)
		}
	}
	result, err = f.request(t.Context(), "get_symbol", map[string]any{"id": current.ID, "format": "json"}, f.srv.handleGetSymbol)
	require.Contains(t, requireContractBody(t, result, err), current.ID)
	result, err = f.request(t.Context(), "get_symbol", map[string]any{"id": removed.ID, "format": "json"}, f.srv.handleGetSymbol)
	require.NotContains(t, requireContractBody(t, result, err), `"kind":"config_key"`)
	binding, pending, err := f.srv.openContractConsumerBinding(t.Context(), nil, "", []string{"repo", "b"})
	require.NoError(t, err)
	require.Nil(t, pending)
	defer binding.close()
	reader, err := binding.analysisReader(t.Context())
	require.NoError(t, err)
	var spring *graph.Edge
	for _, edge := range reader.GetOutEdges(ctor.ID) {
		if edge.Kind == graph.EdgeCalls {
			spring = edge
		}
	}
	require.NotNil(t, spring)
	require.Equal(t, "spring.Bean", spring.Meta["via"])
}

func TestContractConsumerRepoSelectorUsesFullAdmittedCohortAndSharedOwner(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/after")
	f.srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{Request: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error) {
		t.Error("coherent ready cohort scheduled work")
		return false, nil
	}, WaitChange: func(context.Context, graph.ContractAttachmentKey) error {
		t.Error("coherent ready cohort waited")
		return nil
	}})
	result, err := f.request(t.Context(), "search_symbols", map[string]any{"repo": "b", "query": "after", "kind": "contract", "format": "json"}, f.srv.handleSearchSymbols)
	require.Contains(t, requireContractBody(t, result, err), "http::GET::/after", "B's consumer owns the global canonical scalar stored under repo")
	result, err = f.request(t.Context(), "get_symbol", map[string]any{"repo": "b", "id": "http::GET::/after", "format": "json"}, f.srv.handleGetSymbol)
	require.Contains(t, requireContractBody(t, result, err), `"kind":"contract"`)
	result, err = f.request(t.Context(), "graph_query", map[string]any{"repo": "b", "query": "nodes kind=contract", "format": "json"}, f.srv.handleGraphQuery)
	require.Contains(t, requireContractBody(t, result, err), "http::GET::/after")
}

func TestContractConsumerOrdinaryCallersAndTraversalExcludeLegacyDerivedLinks(t *testing.T) {
	f := newContractConsumerFixture(t)
	legacy := &graph.Node{ID: "repo/beans.java::LegacyInjection", Name: "LegacyInjection", Kind: graph.KindFunction, FilePath: "repo/beans.java", RepoPrefix: "repo"}
	table := &graph.Node{ID: "repo/schema.sql::items", Name: "items", Kind: graph.KindTable, FilePath: "repo/schema.sql", RepoPrefix: "repo"}
	f.store.AddBatch([]*graph.Node{legacy, table}, []*graph.Edge{
		{From: legacy.ID, To: "repo/handler.go::Handle", Kind: graph.EdgeCalls, FilePath: legacy.FilePath, Meta: map[string]any{"via": "spring.Bean"}},
		{From: legacy.ID, To: "repo/handler.go::Handle", Kind: graph.EdgeMatches, FilePath: legacy.FilePath},
		{From: "repo/handler.go::Handle", To: table.ID, Kind: graph.EdgeProvides, FilePath: table.FilePath},
	})
	f.srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{Request: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error) {
		t.Error("ordinary traversal scheduled analysis")
		return false, nil
	}, WaitChange: func(context.Context, graph.ContractAttachmentKey) error {
		t.Error("ordinary traversal waited")
		return nil
	}})
	// No input or attachment reader is available. Existing core endpoint rows
	// are the only evidence the adjacency filter needs.
	f.srv.materializer = nil
	ordinary := f.srv.readerFor(t.Context())
	var sqlRows int
	for edge := range ordinary.EdgesByKind(graph.EdgeProvides) {
		require.NotEqual(t, "http::GET::/before", edge.To)
		if edge.To == table.ID {
			sqlRows++
		}
	}
	require.Equal(t, 1, sqlRows, "iterator preserves actual SQL ownership")
	for edge := range ordinary.EdgesByKind(graph.EdgeCalls) {
		require.NotEqual(t, legacy.ID, edge.From)
	}
	result, err := f.request(t.Context(), "get_callers", map[string]any{"id": "repo/handler.go::Handle", "format": "json"}, f.srv.handleGetCallers)
	body := requireContractBody(t, result, err)
	require.Contains(t, body, "InvokeCaller")
	require.NotContains(t, body, "LegacyInjection")
	result, err = f.request(t.Context(), "graph_query", map[string]any{"query": "nodes name~Handle | traverse provides,calls,matches both", "format": "json"}, f.srv.handleGraphQuery)
	// Explicit matches selects strict contract readiness; use a core-only
	// generic traversal to prove callers and ordinary SQL are independent.
	require.NoError(t, err)
	require.True(t, result.IsError)
	result, err = f.request(t.Context(), "graph_query", map[string]any{"query": "nodes name~Handle | traverse provides,calls both", "format": "json"}, f.srv.handleGraphQuery)
	body = requireContractBody(t, result, err)
	require.Contains(t, body, table.ID)
	require.Contains(t, body, "InvokeCaller")
	require.NotContains(t, body, "LegacyInjection")
	require.NotContains(t, body, "http::GET::/before")
}

func TestContractConsumerYieldCountsOrdinaryWorkAndBalancesFailure(t *testing.T) {
	f := newContractConsumerFixture(t)
	runtimeactivity.Begin("mcp")
	defer runtimeactivity.End("mcp")
	f.srv.SetContractAnalysisRuntime(&ContractAnalysisRuntime{Request: func(context.Context, *graphview.RepoView, *graphview.SelectedContractInputs, string, string) (bool, error) {
		if f.srv.ContractAnalysisShouldYield() {
			t.Error("contract waiter self-starved")
		}
		runtimeactivity.Begin("mcp")
		if !f.srv.ContractAnalysisShouldYield() {
			t.Error("ordinary concurrent tool did not retain priority")
		}
		runtimeactivity.End("mcp")
		return true, nil
	}, WaitChange: func(context.Context, graph.ContractAttachmentKey) error { return context.Canceled }})
	_, _, err := f.srv.requestAndWaitContractAnalysis(t.Context(), nil, &pendingContractBinding{})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, f.srv.contractAnalysisWaiters.Load())
	require.True(t, f.srv.ContractAnalysisShouldYield())
}
