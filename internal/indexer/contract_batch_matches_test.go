package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

// Each attachment needs the complete shared bridge group, including matches
// between the other targets, while its owner records remain repository-scoped.
func TestContractFollowupBatchMatchesIndependentPayloadsAndSearch(t *testing.T) {
	fixture := func(t *testing.T) *contractWorkerFixture {
		t.Helper()
		f := newContractWorkerFixture(t)
		add := func(repo, path, source string) {
			t.Helper()
			f.sources[path] = []byte(source)
			extractor, _ := f.registry.GetByLanguage("go")
			parsed, err := extractor.Extract(filepath.Base(path), f.sources[path])
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Tree != nil {
				defer parsed.Tree.Release()
			}
			stripTypeShape(parsed)
			idx := New(graph.New(), f.registry, f.cfg, zap.NewNop())
			defer idx.Close()
			idx.repoPrefix, idx.workspaceID, idx.projectID = repo, "workspace", "project"
			idx.applyRepoPrefix(parsed.Nodes, parsed.Edges)
			if err := f.store.AddBatchChecked(parsed.Nodes, parsed.Edges); err != nil {
				t.Fatal(err)
			}
			policy, err := contractFollowupPolicy(idx, "go")
			if err != nil {
				t.Fatal(err)
			}
			f.files = append(f.files, ContractFollowupFile{RepoPrefix: repo, WorkspaceID: "workspace", ProjectID: "project", Path: path, Language: "go", SourceFingerprint: contractInputHash(f.sources[path]), Policy: policy})
		}
		add("repo-a", "repo-a/extra.go", `package server
import "net/http"
func extra(mux *http.ServeMux) { mux.HandleFunc("GET /v1/items", handleExtra) }
func handleExtra(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }
`)
		add("repo-c", "repo-c/client.go", `package client
import "net/http"
func otherCall() { http.Get("http://other.test/v1/items") }
`)
		state := graph.ContractInputState{RepoPrefix: "repo-c", InputVersion: "worker-boundary-v1", InputFingerprint: "repo-c-accepted"}
		work := graph.ContractWork{Token: "repo-c-token", RepoPrefix: "repo-c", FilePath: "repo-c/client.go", InputVersion: state.InputVersion, InputFingerprint: state.InputFingerprint, State: graph.ContractWorkPending, Scope: f.work["repo-b"].Scope}
		if err := f.store.BeginContractInputMutationContext(context.Background(), nil, state, []graph.ContractWork{work}); err != nil {
			t.Fatal(err)
		}
		if err := f.store.AcceptContractInputMutationContext(context.Background(), state); err != nil {
			t.Fatal(err)
		}
		state.Accepted = true
		f.inputs = append(f.inputs, graph.ContractInputWitness{GenerationID: 0, State: state, Found: true})
		f.work["repo-c"] = work
		return f
	}

	type payload struct {
		nodes     map[string]*graph.Node
		edges     map[string]*graph.Edge
		searchIDs []string
		documents int
	}
	read := func(t *testing.T, req ContractFollowupRequest, report ContractFollowupReport) payload {
		t.Helper()
		var ids []string
		for node := range req.Payload.NodesLightSeq() {
			ids = append(ids, node.ID)
		}
		nodes, err := req.Payload.GetNodesByIDsContext(context.Background(), ids)
		if err != nil {
			t.Fatal(err)
		}
		edges := make(map[string]*graph.Edge)
		matches := 0
		// Match and ownership edges can originate at core symbols that are
		// intentionally not duplicated as nodes in the analysis payload.
		for _, edge := range req.Payload.AllEdges() {
			encoded, err := json.Marshal(edge)
			if err != nil {
				t.Fatal(err)
			}
			edges[string(encoded)+fmt.Sprint(edge.Confidence)] = edge
			if edge.Kind == graph.EdgeMatches {
				matches++
				if !edge.CrossRepo {
					t.Fatal("cross-repository match lost its provenance")
				}
			}
			if edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes || edge.Kind == graph.EdgeHandlesRoute {
				if edge.Meta["contract_owner_repo_prefix"] != req.Snapshot.Key.RepoPrefix {
					t.Fatalf("foreign owner record in target payload: %#v", edge)
				}
			}
		}
		if matches != 4 {
			t.Fatalf("two distinct providers and two consumers must retain all four matches: %d", matches)
		}
		bridge := nodes["bridge::workspace::project::http::GET::/v1/items"]
		if bridge == nil || fmt.Sprint(bridge.Meta["provider_count"]) != "2" || fmt.Sprint(bridge.Meta["consumer_count"]) != "2" {
			t.Fatalf("shared-ID multiplicity lost: %#v", bridge)
		}
		if got, _ := json.Marshal(bridge.Meta["repos"]); string(got) != `["repo-a","repo-b","repo-c"]` {
			t.Fatalf("incomplete bridge membership: %s", got)
		}
		hits, err := req.Payload.SearchSymbolsContext(context.Background(), "items", 100)
		if err != nil {
			t.Fatal(err)
		}
		var searchIDs []string
		for _, hit := range hits {
			searchIDs = append(searchIDs, hit.NodeID)
		}
		sort.Strings(searchIDs)
		if !reflect.DeepEqual(searchIDs, []string{"bridge::workspace::project::http::GET::/v1/items", "http::GET::/v1/items"}) {
			t.Fatalf("shared canonical and bridge search documents missing: %v", searchIDs)
		}
		count, err := req.Payload.SymbolFTSCount()
		if err != nil || count != report.SymbolDocuments {
			t.Fatalf("FTS accounting: count=%d report=%d err=%v", count, report.SymbolDocuments, err)
		}
		mode, known, err := req.Payload.GetSymbolFTSNormalization(req.Snapshot.Key.RepoPrefix)
		if err != nil || !known || mode != search.FTSNormalizationMode() {
			t.Fatalf("target search normalization: %q %v %v", mode, known, err)
		}
		return payload{nodes: nodes, edges: edges, searchIDs: searchIDs, documents: count}
	}

	repos := []string{"repo-a", "repo-b", "repo-c"}
	independent := fixture(t)
	expected := make(map[string]payload)
	for _, repo := range repos {
		req := independent.request(t, repo, "independent-"+repo)
		report, err := RunContractFollowup(context.Background(), req)
		if err != nil || !report.Published {
			t.Fatalf("independent target: %#v %v", report, err)
		}
		expected[repo] = read(t, req, report)
	}

	f := fixture(t)
	var requests []ContractFollowupRequest
	var targets []ContractFollowupTarget
	for _, repo := range repos {
		req := f.request(t, repo, "batch-"+repo)
		requests = append(requests, req)
		targets = append(targets, ContractFollowupTarget{Key: req.Snapshot.Key, Work: req.Snapshot.Work, Payload: req.Payload, Catalog: req.Catalog})
	}
	first := requests[0]
	batch, err := RunContractFollowupBatch(context.Background(), ContractFollowupBatchRequest{Snapshot: first.Snapshot, Targets: targets, RebuildReason: first.RebuildReason, Registry: first.Registry, Config: first.Config, Leases: first.Leases, Yield: first.Yield})
	if err != nil || len(batch.Targets) != len(requests) {
		t.Fatalf("batch targets: %#v %v", batch, err)
	}
	for i, req := range requests {
		result := batch.Targets[i]
		if result.Err != nil || !result.Report.Published {
			t.Fatalf("unpublished target: %#v", result)
		}
		if got := read(t, req, result.Report); !reflect.DeepEqual(got, expected[req.Snapshot.Key.RepoPrefix]) {
			t.Fatalf("batch payload/search differs from independent target %s", req.Snapshot.Key.RepoPrefix)
		}
	}
}
