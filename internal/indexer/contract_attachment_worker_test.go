package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

type contractWorkerFixture struct {
	store    *store_sqlite.Store
	registry *parser.Registry
	cfg      config.IndexConfig
	sources  map[string][]byte
	files    []ContractFollowupFile
	inputs   []graph.ContractInputWitness
	work     map[string]graph.ContractWork
	leases   *graphview.LeaseManager
	releases int
}

func newContractWorkerFixture(t *testing.T) *contractWorkerFixture {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "worker.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	cfg := config.Default().Index
	f := &contractWorkerFixture{store: store, registry: registry, cfg: cfg, sources: map[string][]byte{
		"repo-a/server.go": []byte(`package server
import "net/http"
type Reply struct { Name string ` + "`json:\"name\"`" + ` }
func register(mux *http.ServeMux) { mux.HandleFunc("GET /v1/items", handle) }
func handle(w http.ResponseWriter, r *http.Request) { response := Reply{Name:"ok"}; WriteJSON(w,200,response) }
func WriteJSON(w http.ResponseWriter, code int, body any) {}
`),
		"repo-b/client.go": []byte(`package client
import "net/http"
func call() { http.Get("http://example.test/v1/items") }
`),
	}, work: make(map[string]graph.ContractWork), leases: graphview.NewLeaseManager()}
	extractor, ok := registry.GetByLanguage("go")
	if !ok {
		t.Fatal("missing go extractor")
	}
	for _, repo := range []string{"repo-a", "repo-b"} {
		path := repo + "/server.go"
		if repo == "repo-b" {
			path = repo + "/client.go"
		}
		idx := New(graph.New(), registry, cfg, zap.NewNop())
		idx.repoPrefix = repo
		idx.workspaceID = "workspace"
		idx.projectID = "project"
		result, err := extractor.Extract(filepath.Base(path), f.sources[path])
		if err != nil {
			t.Fatal(err)
		}
		stripTypeShape(result)
		idx.applyRepoPrefix(result.Nodes, result.Edges)
		for _, node := range result.Nodes {
			node.WorkspaceID = "workspace"
			node.ProjectID = "project"
		}
		if err := store.AddBatchChecked(result.Nodes, result.Edges); err != nil {
			t.Fatal(err)
		}
		if result.Tree != nil {
			result.Tree.Release()
		}
		policy, err := contractFollowupPolicy(idx, "go")
		if err != nil {
			t.Fatal(err)
		}
		f.files = append(f.files, ContractFollowupFile{RepoPrefix: repo, WorkspaceID: "workspace", ProjectID: "project", Path: path, Language: "go", SourceFingerprint: contractInputHash(f.sources[path]), Policy: policy})
		state := graph.ContractInputState{RepoPrefix: repo, InputVersion: "worker-boundary-v1", InputFingerprint: repo + "-accepted"}
		work := graph.ContractWork{Token: repo + "-token", RepoPrefix: repo, FilePath: path, InputVersion: state.InputVersion, InputFingerprint: state.InputFingerprint, State: graph.ContractWorkPending, Scope: graph.ContractWorkScope{Groups: []graph.ContractWorkGroup{{WorkspaceID: "workspace", ProjectID: "project", ContractID: "http::GET::/v1/items"}}}}
		if err := store.BeginContractInputMutationContext(context.Background(), nil, state, []graph.ContractWork{work}); err != nil {
			t.Fatal(err)
		}
		if err := store.AcceptContractInputMutationContext(context.Background(), state); err != nil {
			t.Fatal(err)
		}
		state.Accepted = true
		f.inputs = append(f.inputs, graph.ContractInputWitness{GenerationID: 0, State: state, Found: true})
		f.work[repo] = work
	}
	return f
}
func (f *contractWorkerFixture) request(t *testing.T, repo, layer string) ContractFollowupRequest {
	t.Helper()
	state, err := graph.ComposeContractInputState(repo, "", f.inputs)
	if err != nil {
		t.Fatal(err)
	}
	_, payload, err := f.store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "worker", LayerID: layer, GenerationKind: "contract_analysis", ConfigHash: "worker-config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "contract-only", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	req := ContractFollowupRequest{RebuildReason: ContractFollowupColdBaseline, Payload: payload, Catalog: f.store, Registry: f.registry, Config: f.cfg, Leases: f.leases, Yield: func(ctx context.Context) error { return ctx.Err() }}
	req.Snapshot = ContractFollowupSnapshot{Key: graph.ContractAttachmentKey{RepoPrefix: repo, InputVersion: state.InputVersion, InputFingerprint: state.InputFingerprint}, Core: f.store, Inputs: append([]graph.ContractInputWitness(nil), f.inputs...), Work: []graph.ContractWork{f.work[repo]}, Files: append([]ContractFollowupFile(nil), f.files...), Release: func() { f.releases++ }}
	req.Snapshot.ReadAccepted = func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
		if err := ctx.Err(); err != nil {
			return ContractAcceptedSource{}, err
		}
		if !f.leases.InUse(payload.ViewGeneration()) {
			return ContractAcceptedSource{}, fmt.Errorf("analysis payload was not independently leased")
		}
		return ContractAcceptedSource{Bytes: append([]byte(nil), f.sources[file.Path]...), SourceFingerprint: file.SourceFingerprint, Policy: file.Policy}, nil
	}
	req.Snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
		p, err := f.store.LayerContractFileProjectionContext(ctx, file.RepoPrefix, []string{file.Path})
		if err != nil {
			return ContractFollowupCoreFile{}, err
		}
		nodes := p.FileNodes[file.Path]
		var ids []string
		for _, node := range nodes {
			ids = append(ids, node.ID)
		}
		bySource, truncated, err := f.store.GetOutEdgesByNodeIDsContext(ctx, ids, graph.ContractProjectionRowLimit)
		if err != nil {
			return ContractFollowupCoreFile{}, err
		}
		if truncated {
			return ContractFollowupCoreFile{}, graph.ErrContractProjectionLimit
		}
		var edges []*graph.Edge
		for _, rows := range bySource {
			edges = append(edges, rows...)
		}
		return ContractFollowupCoreFile{Nodes: nodes, Edges: edges}, nil
	}
	return req
}

func TestContractFollowupNonemptyCrossRepoShapesAndScopedOwners(t *testing.T) {
	f := newContractWorkerFixture(t)
	a := f.request(t, "repo-a", "analysis-a")
	b := f.request(t, "repo-b", "analysis-b")
	for _, req := range []ContractFollowupRequest{a, b} {
		report, err := RunContractFollowup(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Published || report.Records == 0 || report.SourceReads == 0 || report.SourceBytes == 0 || report.Shapes == 0 {
			t.Fatalf("missing real analysis evidence=%#v", report)
		}
		if f.leases.InUse(report.PayloadGeneration) {
			t.Fatal("payload lease leaked")
		}
	}
	if f.releases != 2 {
		t.Fatalf("selected handoff releases=%d", f.releases)
	}
	var firstContract, firstBridge *graph.Node
	for _, req := range []ContractFollowupRequest{a, b} {
		node := req.Payload.GetNode("http::GET::/v1/items")
		if node == nil {
			t.Fatal("actual shared endpoint missing")
		}
		if firstContract == nil {
			firstContract = node
		} else if !reflect.DeepEqual(firstContract, node) {
			t.Fatal("shared canonical differs across selected cohort attachments")
		}
		var bridges []*graph.Node
		for node := range req.Payload.NodesByKindsSeq(graph.KindContractBridge) {
			bridges = append(bridges, node)
		}
		if len(bridges) != 1 {
			t.Fatalf("real crossrepo bridge count=%d", len(bridges))
		}
		if firstBridge == nil {
			firstBridge = bridges[0]
		} else if !reflect.DeepEqual(firstBridge, bridges[0]) {
			t.Fatal("complete bridge group differs across owners")
		}
		p, err := req.Payload.LayerContractRepoProjectionContext(context.Background(), req.Snapshot.Key.RepoPrefix)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.OwnerRows) == 0 {
			t.Fatal("scoped owner records missing")
		}
		for _, row := range p.OwnerRows {
			if owner, _ := row.Edge.Meta["contract_owner_repo_prefix"].(string); owner != req.Snapshot.Key.RepoPrefix {
				t.Fatalf("foreign owner leaked into exact namespace=%q", owner)
			}
		}
		typ := req.Payload.GetNode("repo-a/server.go::Reply")
		if typ == nil || typ.Meta["shape"] == nil {
			t.Fatal("real accepted response shape missing")
		}
	}
	typ := f.store.GetNode("repo-a/server.go::Reply")
	if typ == nil || typ.Meta["shape"] != nil {
		t.Fatal("analysis mutated ordinary core type metadata")
	}
	if f.store.GetNode("http::GET::/v1/items") != nil {
		t.Fatal("analysis canonical leaked into ordinary core")
	}
}

func TestContractFollowupSourceFailureCancellationAndInputCAS(t *testing.T) {
	for _, mode := range []string{"wrong-bytes", "unavailable", "cancel", "input-race"} {
		t.Run(mode, func(t *testing.T) {
			f := newContractWorkerFixture(t)
			req := f.request(t, "repo-a", mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "wrong-bytes":
				base := req.Snapshot.ReadAccepted
				req.Snapshot.ReadAccepted = func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
					src, err := base(ctx, file)
					src.Bytes = append(src.Bytes, '!')
					return src, err
				}
			case "unavailable":
				req.Snapshot.ReadAccepted = func(context.Context, ContractFollowupFile) (ContractAcceptedSource, error) {
					return ContractAcceptedSource{}, errors.New("accepted historical bytes unavailable")
				}
			case "cancel":
				cancel()
			case "input-race":
				once := false
				req.Yield = func(context.Context) error {
					if !once {
						once = true
						old := f.inputs[1].State
						next := old
						next.InputFingerprint = "companion-changed"
						if err := f.store.BeginContractInputMutationContext(context.Background(), &old, next, nil); err != nil {
							return err
						}
					}
					return nil
				}
			}
			report, err := RunContractFollowup(ctx, req)
			if err == nil || report.Published {
				t.Fatalf("failed proof certified=%#v %v", report, err)
			}
			if f.releases != 1 || f.leases.InUse(req.Payload.ViewGeneration()) {
				t.Fatal("failure leaked handoff/payload lease")
			}
			header, readErr := f.store.GetContractAttachmentContext(context.Background(), req.Snapshot.Key)
			if readErr != nil || header != nil {
				t.Fatalf("failure installed attachment=%#v %v", header, readErr)
			}
			pending, readErr := f.store.PendingContractWorkForScopeContext(context.Background(), "repo-a", "")
			if readErr != nil || len(pending) != 1 || pending[0].State != graph.ContractWorkPending {
				t.Fatalf("failure lost immutable debt=%#v %v", pending, readErr)
			}
		})
	}
}
