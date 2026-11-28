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

// TestIndex_PythonObjectMethodCallsSkipModuleFunctions indexes a package whose
// sibling module defines a free function `get`. An attribute call on an object
// (`d.get(...)`) must not land on it — a dict's get is no module's function —
// while a call through a relative module binding (`from . import helpers`,
// also aliased) still must. Both hold after an incremental re-index too, where
// a caller making both kinds of call must not reuse the module call's binding
// for its object call.
func TestIndex_PythonObjectMethodCallsSkipModuleFunctions(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"pkg/__init__.py": "",
		"pkg/helpers.py": `def get(d, key):
    return d
`,
		"pkg/use.py": `from . import helpers
from . import helpers as h


def lookup(d):
    return d.get("k")


def via_module(d):
    return helpers.get(d, "k")


def via_alias(d):
    return h.get(d, "k")


def mixed(obj):
    helpers.get(obj, "k")
    return obj.get("k")
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

	callsAt := func(from, to string, line int) bool {
		for _, e := range g.GetOutEdges(from) {
			if e.Kind == graph.EdgeCalls && e.To == to && (line == 0 || e.Line == line) {
				return true
			}
		}
		return false
	}
	check := func(phase string) {
		require.False(t, callsAt("pkg/use.py::lookup", "pkg/helpers.py::get", 0), "%s: d.get() must not bind to a module function", phase)
		require.True(t, callsAt("pkg/use.py::via_module", "pkg/helpers.py::get", 0), "%s: helpers.get() through `from . import helpers` must bind", phase)
		require.True(t, callsAt("pkg/use.py::via_alias", "pkg/helpers.py::get", 0), "%s: h.get() through `from . import helpers as h` must bind", phase)
		require.True(t, callsAt("pkg/use.py::mixed", "pkg/helpers.py::get", 18), "%s: helpers.get() in mixed must bind", phase)
		require.False(t, callsAt("pkg/use.py::mixed", "pkg/helpers.py::get", 19), "%s: obj.get() in mixed must not bind", phase)
	}
	check("full index")

	use := filepath.Join(dir, "pkg", "use.py")
	writeFile(t, use, files["pkg/use.py"]+"\n# edited\n")
	require.NoError(t, idx.IndexFile(use))
	check("incremental re-index")
}
