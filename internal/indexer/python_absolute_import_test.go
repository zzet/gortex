package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestIndex_PythonAbsoluteImportCallsResolve indexes a src/-layout package and
// a pytest file that imports from it by absolute module path. Calls through
// `from shop.pricing import ...` must land on the first-party definitions — not
// on `dep::` stubs — so get_callers, get_test_targets and the untested
// analysis see the test.
func TestIndex_PythonAbsoluteImportCallsResolve(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"src/shop/__init__.py": "",
		"src/shop/pricing.py": `def apply_discount(price):
    return price * 0.9


def full_price(price):
    return apply_discount(price) / 0.9


class Cart:
    def total(self):
        return 0
`,
		"tests/test_pricing.py": `import shop.pricing as pricing_mod
from shop.pricing import Cart, apply_discount
from shop.pricing import full_price as full


def test_discount():
    assert apply_discount(100) == 90


def test_full_price_alias():
    assert full(100) == 100


def test_module_attr():
    assert pricing_mod.apply_discount(100) == 90


def test_cart():
    assert Cart().total() == 0
`,
	}
	for rel, src := range files {
		path := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		writeFile(t, path, src)
	}

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewPythonExtractor())
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	hasEdge := func(from string, kind graph.EdgeKind, to string) bool {
		for _, e := range g.GetOutEdges(from) {
			if e.Kind == kind && e.To == to {
				return true
			}
		}
		return false
	}
	cases := []struct{ test, target string }{
		{"tests/test_pricing.py::test_discount", "src/shop/pricing.py::apply_discount"},
		{"tests/test_pricing.py::test_full_price_alias", "src/shop/pricing.py::full_price"},
		{"tests/test_pricing.py::test_module_attr", "src/shop/pricing.py::apply_discount"},
		{"tests/test_pricing.py::test_cart", "src/shop/pricing.py::Cart"},
	}
	for _, c := range cases {
		require.True(t, hasEdge(c.test, graph.EdgeCalls, c.target), "%s must call %s", c.test, c.target)
		require.True(t, hasEdge(c.test, graph.EdgeTests, c.target), "%s must test %s", c.test, c.target)
	}
}
