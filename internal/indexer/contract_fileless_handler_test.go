package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// A route whose handler resolves to a stub with no file, such as a stdlib
// function passed as the handler, keeps the same registration on a full
// lookup, a projected one and an analysis-only read of accepted sources. The
// analysis-only case is the regression: its reader refuses a path outside its
// census, and an empty path used to fail the whole job.
func TestContractProviderHandlerWithoutAFileKeepsItsRegistration(t *testing.T) {
	stub := &graph.Node{ID: "a::stdlib::net/http::NotFound", Name: "NotFound", Kind: graph.KindFunction, RepoPrefix: "a", Language: "go"}
	base := graph.New()
	base.AddNode(stub)
	var want []contracts.Contract
	for _, mode := range []string{"full", "projected", "analysis_only"} {
		t.Run(mode, func(t *testing.T) {
			var store graph.Store = base
			if mode != "full" {
				store = &handlerDeclarationTestStore{Store: base, declarations: map[string][]*graph.Node{"NotFound": {contractHandlerDeclaration(stub)}}}
			}
			idx := &Indexer{graph: store, rootPath: t.TempDir(), repoPrefix: "a", logger: zap.NewNop()}
			if declared, ok := store.(*handlerDeclarationTestStore); ok {
				idx.contractHandlerCandidates = declared.contractHandlerDeclarations
			}
			if mode == "analysis_only" {
				idx.contractAnalysisOnly = true
				idx.contractAcceptedFileSource = func(path string) ([]byte, error) {
					return nil, fmt.Errorf("contract followup: accepted source outside census %q", path)
				}
			}
			reg := contracts.NewRegistry()
			reg.Add(contracts.Contract{ID: "http::GET::/missing", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: "a/router.go", SymbolID: "a/router.go::register", Line: 3, RepoPrefix: "a", Meta: map[string]any{"handler_ident": "http.NotFound"}})
			idx.resolveProviderHandlers(reg)
			require.NoError(t, idx.contractInputError(), "a handler with no file has no source to read")
			got := reg.ByID("http::GET::/missing")[0]
			require.Equal(t, "a/router.go::register", got.SymbolID, "the registration stands")
			require.Equal(t, "a/router.go", got.FilePath)
			require.Equal(t, 3, got.Line)
			if want == nil {
				want = reg.All()
			} else {
				require.Equal(t, want, reg.All(), "every lookup path keeps the same contract")
			}
		})
	}
}

// TestContractJobPublishesARouteWithAStdlibHandler: a job over a router that
// registers a stdlib function as a handler publishes instead of failing on
// the handler's missing source.
func TestContractJobPublishesARouteWithAStdlibHandler(t *testing.T) {
	f := newContractJobFixture(t, map[string]string{
		"routes.go": "package fixture\nimport \"net/http\"\nfunc register(r Router) { r.GET(\"/missing\", http.NotFound); r.GET(\"/users\", users) }\nfunc users() { http.NotFound(nil, nil) }\n",
	})
	report, err := f.job(nil, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	symbols := make(map[string]string)
	for _, c := range f.contracts() {
		for _, route := range []string{"/missing", "/users"} {
			if strings.HasSuffix(c.ID, route) {
				symbols[route], _ = c.Meta["symbol_id"].(string)
			}
		}
	}
	require.Equal(t, map[string]string{"/missing": "fixture/routes.go::register", "/users": "fixture/routes.go::users"}, symbols,
		"the stub route keeps its registration and the real handler is still enriched")
}

// A PHP route with a namespaced controller hydrates every handler candidate
// before selection. A stub with no file that shares the action's name has no
// full row to hydrate; it stays a declaration instead of failing the job, and
// the controller's own method is still chosen.
func TestContractQualifiedClassHydrationSkipsAStubWithoutAFile(t *testing.T) {
	const controller = "a/app/UserController.php"
	class := &graph.Node{ID: controller + "::UserController", Name: "UserController", Kind: graph.KindType, FilePath: controller, RepoPrefix: "a", StartLine: 3, EndLine: 5, Language: "php", Meta: map[string]any{"scope_ns": `App\Http`}}
	method := &graph.Node{ID: controller + "::UserController.delete", Name: "delete", Kind: graph.KindMethod, FilePath: controller, RepoPrefix: "a", StartLine: 4, EndLine: 4, Language: "php"}
	stub := &graph.Node{ID: "b::stdlib::os::delete", Name: "delete", Kind: graph.KindFunction, RepoPrefix: "b", Language: "go"}
	source := "<?php\nnamespace App\\Http;\nclass UserController {\n  public function delete() { return 1; }\n}\n"
	for _, mode := range []string{"projected", "analysis_only"} {
		t.Run(mode, func(t *testing.T) {
			base := graph.New()
			base.AddNode(class)
			base.AddNode(method)
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, "app"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "app", "UserController.php"), []byte(source), 0o600))
			store := &handlerDeclarationTestStore{Store: base, declarations: map[string][]*graph.Node{
				"UserController": {contractHandlerDeclaration(class)},
				"delete":         {contractHandlerDeclaration(stub), contractHandlerDeclaration(method)},
			}}
			idx := &Indexer{graph: store, rootPath: root, repoPrefix: "a", logger: zap.NewNop(), contractHandlerCandidates: store.contractHandlerDeclarations}
			if mode == "analysis_only" {
				idx.contractAnalysisOnly = true
				idx.contractAcceptedFileSource = func(path string) ([]byte, error) {
					switch path {
					case controller:
						return []byte(source), nil
					case "a/routes.php":
						return []byte("<?php\nRoute::delete('/users', [UserController::class, 'delete']);\n"), nil
					}
					return nil, fmt.Errorf("contract followup: accepted source outside census %q", path)
				}
			}
			reg := contracts.NewRegistry()
			reg.Add(contracts.Contract{ID: "http::DELETE::/users", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: "a/routes.php", SymbolID: "a/routes.php::routes", Line: 2, RepoPrefix: "a", Meta: map[string]any{"handler_ident": "delete", "handler_class": `App\Http\UserController`}})
			idx.resolveProviderHandlers(reg)
			require.NoError(t, idx.contractInputError(), "a stub without a file is not a missing owner row")
			require.Equal(t, method.ID, reg.ByID("http::DELETE::/users")[0].SymbolID, "the controller's method is chosen")
		})
	}
}
