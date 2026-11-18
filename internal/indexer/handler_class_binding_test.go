package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestResolveProviderHandlersHandlerClass(t *testing.T) {
	for _, tc := range []struct {
		name, class, action, registration, duplicate, want string
		imports                                            string
	}{
		{"order", "OrderController", "index", "routes/api.php", "", "app/OrderController.php::OrderController.index", ""},
		{"invoice", "InvoiceController", "index", "routes/api.php", "", "app/InvoiceController.php::InvoiceController.index", ""},
		{"name_only_fallback", "InvoiceController", "store", "routes/api.php", "", "app/OrderController.php::OrderController.store", ""},
		{"inherited_action", "InvoiceController", "store", "routes/api.php", "", "app/OrderController.php::OrderController.store", ""},
		{"no_class", "", "index", "api.php", "", "", ""},
		{"missing_class_ambiguous", "MissingController", "index", "api.php", "", "", ""},
		{"missing_class_single_action", "MissingController", "store", "routes/api.php", "", "", ""},
		{"missing_class_local_action", "MissingController", "index", "routes/api.php", "", "", ""},
		{"duplicate_class", "OrderController", "index", "routes/api.php", "other/OrderController.php", "", ""},
		{"duplicate_class_local", "OrderController", "index", "routes/api.php", "routes/OrderController.php", "", ""},
		{"duplicate_class_inherits", "OrderController", "index", "routes/api.php", "other/OrderController.php", "", ""},
		{"alias", "Orders", "index", "routes/api.php", "", "app/OrderController.php::OrderController.index", `use Acme\OrderController as Orders;`},
		{"import_duplicate", "OrderController", "index", "routes/api.php", "other/OrderController.php", "app/OrderController.php::OrderController.index", `use Acme\OrderController;`},
		{"group_alias", "Orders", "index", "routes/api.php", "other/OrderController.php", "app/OrderController.php::OrderController.index", `use Acme\{OrderController as Orders, InvoiceController};`},
		{"namespace", "OrderController", "index", "routes/api.php", "other/OrderController.php", "app/OrderController.php::OrderController.index", `namespace Acme;`},
		{"qualified_alias", `Controllers\OrderController`, "index", "routes/api.php", "other/OrderController.php", "app/OrderController.php::OrderController.index", `use Acme as Controllers;`},
		{"absolute", `\Acme\OrderController`, "index", "routes/api.php", "other/OrderController.php", "app/OrderController.php::OrderController.index", ""},
		{"missing_import", "OrderController", "store", "routes/api.php", "", "", `use Missing\OrderController;`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			core := graph.New()
			files := map[string]string{
				"app/OrderController.php":   "<?php namespace Acme; class OrderController { public function index() {} public function store() {} }",
				"app/InvoiceController.php": "<?php namespace Acme; class InvoiceController { public function index() {} }",
				"routes/Catalog.php":        "<?php namespace Acme; class Catalog { public function index() {} }",
			}
			if tc.name == "inherited_action" {
				files["app/InvoiceController.php"] = "<?php namespace Acme; class InvoiceController extends OrderController { public function index() {} }"
			}
			if tc.duplicate != "" {
				files[tc.duplicate] = "<?php namespace Acme\\Other; class OrderController { public function index() {} }"
				if tc.name == "duplicate_class_inherits" {
					files[tc.duplicate] = "<?php namespace Acme\\Other; class OrderController extends \\Acme\\InvoiceController {}"
				}
			}
			files[tc.registration] = "<?php " + tc.imports + "\n"
			for file, source := range files {
				require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, file)), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, file), []byte(source), 0o600))
				parsed, err := languages.NewPHPExtractor().Extract("a/"+file, []byte(source))
				require.NoError(t, err)
				for _, node := range parsed.Nodes {
					node.RepoPrefix = "a"
				}
				core.AddBatch(parsed.Nodes, parsed.Edges)
			}
			var full []contracts.Contract
			for _, projected := range []bool{false, true} {
				e := completedNameEvidence(t, core)
				e.files = make(map[string]ContractFollowupFile)
				e.handlerSeededFiles = make(map[string]bool)
				for file := range files {
					file = "a/" + file
					e.files[file] = ContractFollowupFile{RepoPrefix: "a", Path: file}
					e.handlerSeededFiles[file] = true
				}
				counting := &residualBatchCountingStore{Store: e}
				idx := &Indexer{graph: counting, rootPath: root, repoPrefix: "a", logger: zap.NewNop()}
				if projected {
					idx.contractHandlerCandidates = func(names []string) (map[string][]*graph.Node, bool) {
						rows, handled := e.contractHandlerDeclarations(names)
						require.True(t, handled, "use the real declaration projection")
						return rows, handled
					}
				}
				reg := contracts.NewRegistry()
				reg.Add(contracts.Contract{ID: "http::route", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
					FilePath: "a/" + tc.registration, RepoPrefix: "a", Line: 7,
					Meta: map[string]any{"handler_ident": tc.action, "handler_class": tc.class}})
				idx.resolveProviderHandlers(reg)
				require.NoError(t, e.err)
				require.NoError(t, idx.contractInputError())
				want := tc.want
				if want != "" {
					want = "a/" + want
				}
				require.Equal(t, want, reg.All()[0].SymbolID, "projected=%t", projected)
				require.Zero(t, counting.findNodesByNameCalls)
				require.Zero(t, counting.getNodeCalls)
				require.LessOrEqual(t, counting.getNodesByIDsCalls, 1)
				require.LessOrEqual(t, counting.getFileNodesByPathsCalls, 1)
				if projected {
					require.Zero(t, counting.findNodesByNamesCalls)
					require.Equal(t, full, reg.All())
				} else {
					require.Equal(t, 1, counting.findNodesByNamesCalls)
					full = reg.All()
				}
			}
		})
	}
}

func TestLaravelResourceOptionsHandlerBinding(t *testing.T) {
	for _, tc := range []struct{ name, importLine, controller, duplicate, want string }{
		{"imported", `use Acme\ShopController;`, "ShopController", "", "app/ShopController.php::ShopController.index"},
		{"ambiguous", `use Acme\ShopController;`, "ShopController", "other/ShopController.php", ""},
		{"missing", `use Acme\MissingController;`, "MissingController", "", ""},
		{"no_import", "", "ShopController", "", "app/ShopController.php::ShopController.index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			core := graph.New()
			files := map[string]string{
				"app/ShopController.php":      "<?php namespace Acme; class ShopController { public function index() {} }",
				"other/CatalogController.php": "<?php namespace Acme\\Other; class CatalogController { public function index() {} }",
			}
			if tc.duplicate != "" {
				files[tc.duplicate] = "<?php namespace Acme\\Other; class ShopController { public function index() {} }"
			}
			for file, source := range files {
				require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, file)), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, file), []byte(source), 0o600))
				parsed, err := languages.NewPHPExtractor().Extract("a/"+file, []byte(source))
				require.NoError(t, err)
				for _, node := range parsed.Nodes {
					node.RepoPrefix = "a"
				}
				core.AddBatch(parsed.Nodes, parsed.Edges)
			}
			source := "<?php\n" + tc.importLine + "\nRoute::resource('orders', " + tc.controller + "::class, ['names' => 'orders'])->only(['index']);\n"
			cs := (&contracts.HTTPExtractor{}).Extract("a/routes/api.php", []byte(source), nil, nil)
			require.Len(t, cs, 1, "resource options must retain the index route")
			reg := contracts.NewRegistry()
			cs[0].RepoPrefix = "a"
			reg.Add(cs[0])
			idx := &Indexer{graph: core, rootPath: root, repoPrefix: "a", logger: zap.NewNop()}
			idx.resolveProviderHandlers(reg)
			want := tc.want
			if want != "" {
				want = "a/" + want
			}
			require.Equal(t, want, reg.All()[0].SymbolID)
		})
	}
}
