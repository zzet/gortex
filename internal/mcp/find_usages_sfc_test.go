package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"go.uber.org/zap"
)

// A .vue <script setup> that imports a .ts composable and calls it must show
// up in find_usages on the .ts symbol, and a component-to-component
// `import Foo from './Foo.vue'` must bind to the SFC file, not an external
// stub. End-to-end through the real indexer + resolver + handler.
func setupSFCServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}
	write("useX.ts", "export function useX() { return 1 }\n")
	write("Foo.vue", "<script setup lang=\"ts\">\nimport { useX } from './useX'\nconst x = useX()\n</script>\n<template><div>{{ x }}</div></template>\n")
	write("Bar.vue", "<script setup lang=\"ts\">\nimport Foo from './Foo.vue'\n</script>\n<template><Foo/></template>\n")

	g := graph.New()
	cfg := config.Default()
	idx := indexer.New(g, testRegistry(), cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	return NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
}

func TestFindUsages_TSSymbolCalledFromVueScript(t *testing.T) {
	srv := setupSFCServer(t)
	res := callTool(t, srv, "find_usages", map[string]any{"id": "useX.ts::useX", "group_by": "file"})
	require.False(t, res.IsError, resultText(res))
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(res)), &resp))
	files := map[string]bool{}
	for _, grp := range resp["groups"].([]any) {
		files[grp.(map[string]any)["file"].(string)] = true
	}
	require.True(t, files["Foo.vue"], "find_usages useX.ts::useX should list the Foo.vue call site; got %v", resp)
}

func TestVueImportsVueBindsToFile(t *testing.T) {
	srv := setupSFCServer(t)
	var resolved bool
	for _, e := range srv.graph.GetOutEdges("Bar.vue") {
		if e.Kind == graph.EdgeImports && e.To == "Foo.vue" {
			resolved = true
		}
	}
	require.True(t, resolved, "Bar.vue -> Foo.vue import edge should bind to the Foo.vue file node")
}
