package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestVueTemplateReferenceImport(t *testing.T) {
	for _, lang := range []string{"js", "ts"} {
		for _, tc := range []struct {
			name, script, tag, want string
		}{
			{"renamed default", `import Panel from './shop/Widget.vue';`, "Panel", "shop/Widget.vue::Widget"},
			{"collision", `import Widget from './shop/Widget.vue';`, "Widget", "shop/Widget.vue::Widget"},
			{"missing Vue target with PHP collision", `import Widget from './missing/Widget.vue';`, "Widget", "unresolved::Widget"},
			{"missing Vue target with TS collision", `import Panel from './missing/Panel.vue';`, "Panel", "unresolved::Panel"},
			{"external import", `import Widget from 'acme-ui';`, "Widget", "unresolved::Widget"},
			{"ambiguous script blocks", `import Widget from './shop/Widget.vue';
</script><script lang="` + lang + `">
import Widget from './archive/Widget.vue';`, "Widget", "unresolved::Widget"},
			{"vite alias", `import Widget from '@/shop/Widget.vue';`, "Widget", "shop/Widget.vue::Widget"},
			{"nuxt alias", `import Widget from '~/shop/Widget.vue';`, "Widget", "shop/Widget.vue::Widget"},
			{"extensionless", `import Widget from './shop/Widget';`, "Widget", "shop/Widget.vue::Widget"},
			{"javascript", `import Panel from './Panel.js';`, "Panel", "Panel.js::Panel"},
			{"typescript", `import Panel from './Panel.ts';`, "Panel", "Panel.ts::Panel"},
			{"lazy binding", `import LazyPanel from './shop/Widget.vue';`, "LazyPanel", "shop/Widget.vue::Widget"},
			{"kebab binding", `import MyPanel from './shop/Widget.vue';`, "my-panel", "shop/Widget.vue::Widget"},
			{"unimported type unchanged", "", "Widget", "Widget.php::Widget"},
			{"no import", "", "UnknownPanel", "unresolved::UnknownPanel"},
		} {
			t.Run(lang+"/"+tc.name, func(t *testing.T) {
				g := buildGraphFromSources(t, map[string]string{
					"Shop.vue":           "<script setup lang=\"" + lang + "\">\n" + tc.script + "\n</script>\n<template><" + tc.tag + " /></template>",
					"shop/Widget.vue":    `<template><div /></template>`,
					"archive/Widget.vue": `<template><div /></template>`,
					"Panel.js":           `export default function Panel() {}`,
					"Panel.ts":           `export default class Panel {}`,
				})
				g.AddNode(&graph.Node{ID: "Widget.php::Widget", Name: "Widget", Kind: graph.KindType, Language: "php", FilePath: "Widget.php"})
				New(g).ResolveAll()
				var found bool
				for _, e := range g.GetOutEdges("Shop.vue::Shop") {
					if e.Kind != graph.EdgeReferences || e.Meta["template"] != true {
						continue
					}
					found = true
					if e.To != tc.want {
						t.Errorf("template reference = %q, want %q", e.To, tc.want)
					}
				}
				if !found {
					t.Fatal("missing template reference")
				}
			})
		}
	}
}

func TestVueTemplateReferenceImportSQLite(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"Shop.vue":        "<script setup>\nimport Panel from './shop/Widget.vue'\n</script>\n<template><Panel /></template>",
		"shop/Widget.vue": `<template><div /></template>`,
	})
	path := t.TempDir() + "/graph.sqlite"
	s, err := store_sqlite.Open(path)
	require.NoError(t, err)
	for _, n := range g.AllNodes() {
		s.AddNode(n)
	}
	for _, n := range g.AllNodes() {
		for _, e := range g.GetOutEdges(n.ID) {
			s.AddEdge(e)
		}
	}
	require.NoError(t, s.Close())
	s, err = store_sqlite.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	imports := s.GetOutEdges("Shop.vue")
	require.NotEmpty(t, imports)
	persisted := false
	for _, imp := range imports {
		if imp.Meta["vue_default_import"] == "Panel" {
			persisted = true
			require.Empty(t, imp.Alias)
		}
	}
	require.True(t, persisted, "default binding must survive SQLite round trip")
	New(s).ResolveAll()
	found := false
	for _, e := range s.GetOutEdges("Shop.vue::Shop") {
		if e.Kind == graph.EdgeReferences && e.Meta["template"] == true {
			found = true
			require.Equal(t, "shop/Widget.vue::Widget", e.To)
		}
	}
	require.True(t, found)
}

func TestVueTemplateReferenceExternalImport(t *testing.T) {
	for _, spec := range []string{"@/shop/Widget.vue", "~/shop/Widget.vue", "./shop/Widget", "./missing/Widget.vue"} {
		t.Run(spec, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"Shop.vue":           "<script setup>\nimport Widget from '" + spec + "'\n</script>\n<template><Widget /></template>",
				"shop/Widget.vue":    `<template><div /></template>`,
				"archive/Widget.vue": `<template><div /></template>`,
			})
			for _, e := range g.GetOutEdges("Shop.vue") {
				if e.Kind == graph.EdgeImports && strings.HasPrefix(e.To, "unresolved::import::") {
					e.To = "external::" + strings.TrimPrefix(e.To, "unresolved::import::")
				}
			}
			New(g).ResolveAll()
			want := "shop/Widget.vue::Widget"
			if strings.Contains(spec, "missing/") {
				want = "unresolved::Widget"
			}
			found := false
			for _, e := range g.GetOutEdges("Shop.vue::Shop") {
				if e.Kind == graph.EdgeReferences && e.Meta["template"] == true {
					found = true
					require.Equal(t, want, e.To)
				}
			}
			require.True(t, found)
		})
	}
}
