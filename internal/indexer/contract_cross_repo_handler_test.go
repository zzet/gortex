package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// A route whose handler name matches nothing in its own repository but a
// function in another one keeps its registration: an owner in another
// repository is not an owner the route's namespace can hold. The regression is
// the analysis-only read of accepted sources, the one path that can read the
// other repository's file; the full and projected lookups, which cannot, keep
// the same contract.
func TestContractProviderHandlerInAnotherRepoKeepsItsRegistration(t *testing.T) {
	other := &graph.Node{ID: "b/api/token.go::token", Name: "token", Kind: graph.KindFunction, FilePath: "b/api/token.go", RepoPrefix: "b", StartLine: 1, EndLine: 1, Language: "go"}
	base := graph.New()
	base.AddNode(other)
	var want []contracts.Contract
	for _, mode := range []string{"full", "projected", "analysis_only"} {
		t.Run(mode, func(t *testing.T) {
			var store graph.Store = base
			idx := &Indexer{graph: store, rootPath: t.TempDir(), repoPrefix: "a", logger: zap.NewNop()}
			if mode != "full" {
				declared := &handlerDeclarationTestStore{Store: base, declarations: map[string][]*graph.Node{"token": {contractHandlerDeclaration(other)}}}
				idx.graph = declared
				idx.contractHandlerCandidates = declared.contractHandlerDeclarations
			}
			if mode == "analysis_only" {
				idx.contractAnalysisOnly = true
				idx.contractAcceptedFileSource = func(path string) ([]byte, error) {
					return []byte("package api\nimport \"net/http\"\nfunc token(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }\n"), nil
				}
			}
			reg := contracts.NewRegistry()
			reg.Add(contracts.Contract{ID: "http::GET::/auth/token", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: "a/routes.js", SymbolID: "a/routes.js::register", Line: 4, RepoPrefix: "a", Meta: map[string]any{"handler_ident": "token"}})
			idx.resolveProviderHandlers(reg)
			require.NoError(t, idx.contractInputError())
			got := reg.ByID("http::GET::/auth/token")[0]
			require.Equal(t, "a/routes.js::register", got.SymbolID, "the registration stands")
			require.Equal(t, "a/routes.js", got.FilePath)
			require.Equal(t, 4, got.Line)
			if want == nil {
				want = reg.All()
			} else {
				require.Equal(t, want, reg.All(), "every lookup path keeps the same contract")
			}
		})
	}
}

// TestContractJobPublishesARouteWhoseHandlerNameIsInAnotherRepo: a cohort
// where one repository registers a route under a handler name that only
// another repository declares publishes, instead of failing the whole job on
// an owner outside the route's repository.
func TestContractJobPublishesARouteWhoseHandlerNameIsInAnotherRepo(t *testing.T) {
	f := newContractWorkerFixture(t)
	add := func(repo, path, source string) {
		t.Helper()
		f.sources[path] = []byte(source)
		extractor, _ := f.registry.GetByLanguage("go")
		parsed, err := extractor.Extract(filepath.Base(path), f.sources[path])
		require.NoError(t, err)
		if parsed.Tree != nil {
			defer parsed.Tree.Release()
		}
		stripTypeShape(parsed)
		idx := New(graph.New(), f.registry, f.cfg, zap.NewNop())
		defer idx.Close()
		idx.repoPrefix, idx.workspaceID, idx.projectID = repo, "workspace", "project"
		idx.applyRepoPrefix(parsed.Nodes, parsed.Edges)
		for _, node := range parsed.Nodes {
			node.WorkspaceID, node.ProjectID = "workspace", "project"
		}
		require.NoError(t, f.store.AddBatchChecked(parsed.Nodes, parsed.Edges))
		policy, err := contractFollowupPolicy(idx, "go")
		require.NoError(t, err)
		f.files = append(f.files, ContractFollowupFile{RepoPrefix: repo, WorkspaceID: "workspace", ProjectID: "project", Path: path, Language: "go", SourceFingerprint: contractInputHash(f.sources[path]), Policy: policy})
	}
	add("repo-a", "repo-a/auth.go", `package server
import "net/http"
func auth(mux *http.ServeMux) { mux.HandleFunc("GET /auth/token", token) }
`)
	add("repo-b", "repo-b/token.go", `package client
import "net/http"
func token(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }
`)
	for _, repo := range []string{"repo-a", "repo-b"} {
		req := f.request(t, repo, "analysis-"+repo)
		report, err := RunContractFollowup(context.Background(), req)
		require.NoError(t, err, "a handler name only another repository declares does not fail the job")
		require.True(t, report.Published)
		if repo != "repo-a" {
			continue
		}
		node := req.Payload.GetNode("http::GET::/auth/token")
		require.NotNil(t, node, "the route is published")
		require.Equal(t, "repo-a/auth.go::auth", node.Meta["symbol_id"], "it keeps its registration")
	}
}
