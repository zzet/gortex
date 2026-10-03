package tsalias

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoad_Extends(t *testing.T) {
	cases := []struct {
		name    string
		configs map[string]string
		want    string
	}{
		{
			name: "generated Nuxt config keeps declaring target base and root scope",
			configs: map[string]string{
				"tsconfig.json":       `{"extends":"./.nuxt/tsconfig.json"}`,
				".nuxt/tsconfig.json": `{"compilerOptions":{"paths":{"~/*":["../app/*"]}}}`,
			},
			want: "app/composables/auth",
		},
		{
			name: "extensionless parent inherits baseUrl",
			configs: map[string]string{
				"tsconfig.json":    `{"extends":"./config/base","compilerOptions":{"paths":{"~/*":["*"]}}}`,
				"config/base.json": `{"compilerOptions":{"baseUrl":"../app"}}`,
			},
			want: "app/composables/auth",
		},
		{
			name: "child baseUrl rebases inherited paths",
			configs: map[string]string{
				"tsconfig.json":    `{"extends":"./config/base.json","compilerOptions":{"baseUrl":"app"}}`,
				"config/base.json": `{"compilerOptions":{"baseUrl":"../old","paths":{"~/*":["*"]}}}`,
			},
			want: "app/composables/auth",
		},
		{
			name: "last extends entry overrides earlier paths without merging",
			configs: map[string]string{
				"tsconfig.json":      `{"extends":["./config/first.json","./config/second.json"]}`,
				"config/first.json":  `{"compilerOptions":{"paths":{"~/*":["../old/*"],"first/*":["first/*"]}}}`,
				"config/second.json": `{"compilerOptions":{"paths":{"~/*":["../app/*"]}}}`,
			},
			want: "app/composables/auth",
		},
		{
			name: "package specifiers do not suppress child paths",
			configs: map[string]string{
				"tsconfig.json": `{"extends":"@nuxt/tsconfig","compilerOptions":{"paths":{"~/*":["app/*"]}}}`,
			},
			want: "app/composables/auth",
		},
		{
			name: "cycles retain declared child paths",
			configs: map[string]string{
				"tsconfig.json":    `{"extends":"./config/base.json","compilerOptions":{"paths":{"~/*":["app/*"]}}}`,
				"config/base.json": `{"extends":"../tsconfig.json"}`,
			},
			want: "app/composables/auth",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, contents := range tc.configs {
				tsaliasWrite(t, filepath.Join(root, filepath.FromSlash(name)), contents)
			}
			coll := Load(root)
			require.NotNil(t, coll)
			m := coll.FindForFile("app/pages/index.vue")
			require.NotNil(t, m)
			require.Equal(t, tc.want, Resolve(m, "~/composables/auth"))
			require.Empty(t, Resolve(m, "first/only"), "inherited paths are replaced, not merged")
		})
	}
}

func TestLoad_ExtendsChildPathsReplaceParent(t *testing.T) {
	root := t.TempDir()
	tsaliasWrite(t, filepath.Join(root, "tsconfig.json"), `{"extends":"./base.json","compilerOptions":{"paths":{}}}`)
	tsaliasWrite(t, filepath.Join(root, "base.json"), `{"compilerOptions":{"baseUrl":".","paths":{"~/*":["app/*"]}}}`)
	coll := Load(root)
	require.NotNil(t, coll)
	require.Empty(t, Resolve(coll.FindForFile("app/page.ts"), "~/composables/auth"))
}
