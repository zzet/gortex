package mcp

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// syncMapLoadLine returns the file and declaration line of
// `func (m *Map) Load` in the host toolchain's sync/map.go, so the
// fixture matches the real stdlib whatever Go version is installed.
func syncMapLoadLine(t *testing.T) (string, int) {
	t.Helper()
	mapGo := filepath.Join(runtime.GOROOT(), "src", "sync", "map.go")
	require.FileExists(t, mapGo)
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, mapGo, nil, 0)
	require.NoError(t, err)
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Name.Name != "Load" {
			continue
		}
		return mapGo, fset.Position(fd.Pos()).Line
	}
	t.Fatal("no (m *Map) Load declaration found in sync/map.go")
	return "", 0
}

// confineToTestRoot points the external source confinement at root for
// the duration of the test.
func confineToTestRoot(t *testing.T, root string) {
	t.Helper()
	orig := externalSourceRoots
	externalSourceRoots = func() []string { return []string{root} }
	t.Cleanup(func() { externalSourceRoots = orig })
}

func TestGetSymbolSourceExternalStdlib(t *testing.T) {
	srv, _, _ := newIsolationServer(t)
	mapGo, loadLine := syncMapLoadLine(t)

	const ext = "ext::go:sync::Map.Load"
	srv.graph.AddNode(&graph.Node{
		ID: ext, Kind: graph.KindMethod, Name: "Load",
		FilePath: "external::go:sync", Language: "go",
		Meta: map[string]any{
			"external":    true,
			"module_kind": "stdlib",
			"module_path": "sync",
			"signature":   "func (m *Map) Load(key any) (value any, ok bool)",
			"source_file": mapGo,
			"source_line": loadLine,
		},
	})

	res, err := srv.handleGetSymbolSource(t.Context(),
		makeReq("get_symbol_source", map[string]any{"id": ext, "context_lines": 0, "format": "json"}))
	require.NoError(t, err)
	out := extractTextResult(t, res)

	assert.Equal(t, true, out["external"])
	assert.Equal(t, true, out["read_only"])
	assert.Equal(t, "stdlib", out["module_kind"])
	assert.Equal(t, "sync", out["module_path"])
	assert.Equal(t, "external::go:sync", out["file_path"])
	source, _ := out["source"].(string)
	require.Contains(t, source, "func (m *Map) Load(")
	assert.LessOrEqual(t, out["start_line"].(float64), float64(loadLine))
	assert.GreaterOrEqual(t, out["end_line"].(float64), float64(loadLine))
}

func TestGetSymbolSourceExternalModuleCacheMissingDegrades(t *testing.T) {
	srv, _, _ := newIsolationServer(t)
	root := t.TempDir()
	confineToTestRoot(t, root)

	const ext = "ext::go:golang.org/x/tools/go/packages::Config"
	srv.graph.AddNode(&graph.Node{
		ID: ext, Kind: graph.KindType, Name: "Config",
		FilePath: "external::go:golang.org/x/tools/go/packages", Language: "go",
		Meta: map[string]any{
			"external":    true,
			"module_kind": "module_cache",
			"module_path": "golang.org/x/tools",
			"version":     "v0.52.0",
			"signature":   "type Config struct{...}",
			// Inside the allowed root, but the cache entry is gone.
			"source_file": filepath.Join(root, "golang.org/x/tools@v0.52.0", "go", "packages", "config.go"),
			"source_line": 42,
		},
	})

	res, err := srv.handleGetSymbolSource(t.Context(),
		makeReq("get_symbol_source", map[string]any{"id": ext, "format": "json"}))
	require.NoError(t, err)
	out := extractTextResult(t, res)

	assert.Equal(t, true, out["external"])
	assert.Equal(t, true, out["read_only"])
	assert.Nil(t, out["source"])
	reason, _ := out["source_unavailable_reason"].(string)
	require.NotEmpty(t, reason, "degraded answer must say why there is no source")
	assert.Equal(t, "module_cache", out["module_kind"])
	assert.Equal(t, "v0.52.0", out["version"])
}

func TestExternalSourcePathConfined(t *testing.T) {
	srv, _, _ := newIsolationServer(t)
	root := t.TempDir()
	confineToTestRoot(t, root)

	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "secret.go")
	require.NoError(t, os.WriteFile(secret, []byte("package secret\n\nfunc Hidden() {}\n"), 0o600))

	const ext = "ext::go:evil::Steal"
	srv.graph.AddNode(&graph.Node{
		ID: ext, Kind: graph.KindFunction, Name: "Steal",
		FilePath: "external::go:evil", Language: "go",
		Meta: map[string]any{
			"external":    true,
			"module_kind": "module_cache",
			"source_file": secret, // outside every allowed root
			"source_line": 3,
		},
	})

	res, err := srv.handleGetSymbolSource(t.Context(),
		makeReq("get_symbol_source", map[string]any{"id": ext, "format": "json"}))
	require.NoError(t, err)
	out := extractTextResult(t, res)
	assert.Nil(t, out["source"])
	assert.NotEmpty(t, out["source_unavailable_reason"])
	require.NotContains(t, resultText(res), "Hidden")

	// The same path reached through a symlink inside the root is
	// refused the same way.
	link := filepath.Join(root, "link.go")
	require.NoError(t, os.Symlink(secret, link))
	srv.graph.AddNode(&graph.Node{
		ID: ext + ".link", Kind: graph.KindFunction, Name: "Steal",
		FilePath: "external::go:evil", Language: "go",
		Meta: map[string]any{
			"external":    true,
			"source_file": link,
			"source_line": 3,
		},
	})
	res, err = srv.handleGetSymbolSource(t.Context(),
		makeReq("get_symbol_source", map[string]any{"id": ext + ".link", "format": "json"}))
	require.NoError(t, err)
	out = extractTextResult(t, res)
	assert.Nil(t, out["source"])
	assert.NotEmpty(t, out["source_unavailable_reason"])
	require.NotContains(t, resultText(res), "Hidden")
}

func TestNavReadExternalSymbol(t *testing.T) {
	srv, _, _ := newIsolationServer(t)
	mapGo, loadLine := syncMapLoadLine(t)

	const ext = "ext::go:sync::Map.Load"
	srv.graph.AddNode(&graph.Node{
		ID: ext, Kind: graph.KindMethod, Name: "Load",
		FilePath: "external::go:sync", Language: "go",
		Meta: map[string]any{
			"external":    true,
			"module_kind": "stdlib",
			"module_path": "sync",
			"source_file": mapGo,
			"source_line": loadLine,
		},
	})

	gotoRes, err := srv.handleNav(t.Context(), makeReq("nav", map[string]any{"action": "goto", "id": ext}))
	require.NoError(t, err)
	require.False(t, gotoRes.IsError, resultText(gotoRes))
	readRes, err := srv.handleNav(t.Context(), makeReq("nav", map[string]any{"action": "read"}))
	require.NoError(t, err)
	out := extractTextResult(t, readRes)

	assert.Equal(t, true, out["external"])
	assert.Equal(t, true, out["read_only"])
	source, _ := out["source"].(string)
	require.Contains(t, source, "func (m *Map) Load(")
	_, hasNote := out["note"]
	assert.False(t, hasNote, "external reads must not answer with the no-line-range note")

	// Without a recorded position the read degrades instead of erroring.
	const bare = "ext::go:os::Getenv"
	srv.graph.AddNode(&graph.Node{
		ID: bare, Kind: graph.KindFunction, Name: "Getenv",
		FilePath: "external::go:os", Language: "go",
		Meta: map[string]any{"external": true, "module_kind": "stdlib"},
	})
	_, err = srv.handleNav(t.Context(), makeReq("nav", map[string]any{"action": "goto", "id": bare}))
	require.NoError(t, err)
	readRes, err = srv.handleNav(t.Context(), makeReq("nav", map[string]any{"action": "read"}))
	require.NoError(t, err)
	out = extractTextResult(t, readRes)
	assert.Nil(t, out["source"])
	assert.NotEmpty(t, out["source_unavailable_reason"])
	_, hasNote = out["note"]
	assert.False(t, hasNote)
}

func TestEditPathsRefuseExternalSymbols(t *testing.T) {
	srv, _, _ := newIsolationServer(t)
	const ext = "ext::go:sync::Map.Load"
	srv.graph.AddNode(&graph.Node{
		ID: ext, Kind: graph.KindMethod, Name: "Load",
		FilePath: "external::go:sync", Language: "go",
		Meta: map[string]any{"external": true, "module_kind": "stdlib"},
	})

	editRes, err := srv.handleEditSymbol(t.Context(),
		makeReq("edit_symbol", map[string]any{"id": ext, "old_source": "a", "new_source": "b"}))
	require.NoError(t, err)
	require.True(t, editRes.IsError, "edit_symbol must refuse an external symbol")
	assert.Contains(t, resultText(editRes), "is external and read-only")

	deleteRes, err := srv.handleSafeDeleteSymbol(t.Context(),
		makeReq("safe_delete_symbol", map[string]any{"id": ext}))
	require.NoError(t, err)
	require.True(t, deleteRes.IsError, "safe_delete_symbol must refuse an external symbol")
	assert.Contains(t, resultText(deleteRes), "is external and read-only")

	_, cfgErr := srv.buildSymbolCFG(t.Context(), ext)
	require.Error(t, cfgErr, "get_cfg must refuse an external symbol")
	assert.Contains(t, cfgErr.Error(), "is external and read-only")

	// A repo symbol without a line range keeps the existing message.
	const bare = "main.go::NoRange"
	srv.graph.AddNode(&graph.Node{
		ID: bare, Kind: graph.KindFunction, Name: "NoRange",
		FilePath: "main.go", Language: "go",
	})
	editRes, err = srv.handleEditSymbol(t.Context(),
		makeReq("edit_symbol", map[string]any{"id": bare, "old_source": "a", "new_source": "b"}))
	require.NoError(t, err)
	require.True(t, editRes.IsError)
	assert.Contains(t, resultText(editRes), "symbol has no line range: "+bare)
}
