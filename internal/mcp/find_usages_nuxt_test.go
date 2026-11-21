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

func TestFindUsages_NuxtAutoImports(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"tsconfig.json":           `{"extends":"./.nuxt/tsconfig.json"}`,
		".nuxt/tsconfig.json":     `{"compilerOptions":{"paths":{"~/*":["../app/*"]}}}`,
		"app/pages/explicit.ts":   "import { useRedirect } from '~/composables/auth'\nexport function run() { return useRedirect() }\n",
		"app/pages/explicit.vue":  "<script setup lang=\"ts\">\nimport { useRedirect } from '~/composables/auth'\nconst result = useRedirect()\n</script>\n<template><div>{{ result }}</div></template>\n",
		"app/pages/mock.ts":       "vi.mock('~/composables/auth', () => ({ useRedirect: () => ({}) }))\n",
		".nuxt/imports.d.ts":      "export { useRedirect, useRedirect as redirect } from '../app/composables/auth';\nexport { foreign } from 'external-package';\n",
		"app/composables/auth.ts": "export const useRedirect = () => 1\n",
		"app/pages/auto.vue":      "<script setup lang=\"ts\">\nconst result = useRedirect()\n</script>\n<template><div>{{ result }}</div></template>\n",
		"app/pages/renamed.ts":    "export function run() { return redirect() }\n",
		"app/pages/local.ts":      "function useRedirect() { return 2 }\nexport function run() { return useRedirect() }\n",
		"server/api/redirect.ts":  "export function run() { return useRedirect() }\n",
		"app/pages/comment.vue":   "<script setup lang=\"ts\">\n// useRedirect()\n</script>\n<template><div/></template>\n",
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	g := graph.New()
	cfg := config.Default()
	idx := indexer.New(g, testRegistry(), cfg.Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err := idx.Index(root)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	res := callTool(t, srv, "find_usages", map[string]any{"id": "app/composables/auth.ts::useRedirect", "group_by": "file", "context": "call"})
	require.False(t, res.IsError, resultText(res))
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(res)), &resp))
	found := map[string]bool{}
	for _, grp := range resp["groups"].([]any) {
		found[grp.(map[string]any)["file"].(string)] = true
	}
	require.Equal(t, map[string]bool{"app/pages/auto.vue": true, "app/pages/renamed.ts": true, "app/pages/explicit.ts": true, "app/pages/explicit.vue": true}, found)
	for _, file := range []string{"app/pages/auto.vue", "app/pages/renamed.ts"} {
		var resolved bool
		for _, edge := range g.GetInEdges("app/composables/auth.ts::useRedirect") {
			if edge.Kind == graph.EdgeCalls && edge.FilePath == file {
				resolved = true
				require.Equal(t, graph.OriginASTResolved, edge.Origin)
				require.Equal(t, "nuxt_auto_import", edge.Meta["resolution"])
			}
		}
		require.True(t, resolved, "missing generated auto-import call from %s", file)
	}
	for _, file := range []string{"app/pages/explicit.ts", "app/pages/explicit.vue"} {
		var calls int
		for _, edge := range g.GetInEdges("app/composables/auth.ts::useRedirect") {
			if edge.Kind == graph.EdgeCalls && edge.FilePath == file {
				calls++
				require.Equal(t, graph.OriginASTResolved, edge.Origin)
				require.Equal(t, "import_binding", edge.Meta["resolution"])
			}
		}
		require.Equal(t, 1, calls, "each explicit-import caller must resolve once")
	}
}
