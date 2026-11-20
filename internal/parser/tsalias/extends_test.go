package tsalias

import (
	"fmt"
	"os"
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
			name: "dotted stem falls back to json",
			configs: map[string]string{
				"tsconfig.json":             `{"extends":"./config/tsconfig.base"}`,
				"config/tsconfig.base.json": `{"compilerOptions":{"paths":{"~/*":["../app/*"]}}}`,
			},
			want: "app/composables/auth",
		},
		{
			name: "literal parent wins over json fallback",
			configs: map[string]string{
				"tsconfig.json": `{"extends":"./base"}`,
				"base":          `{"compilerOptions":{"paths":{"~/*":["app/*"]}}}`,
				"base.json":     `{"compilerOptions":{"paths":{"~/*":["wrong/*"]}}}`,
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
			for _, coll := range []*Collection{Load(root), LoadTree(memTree{files: tc.configs})} {
				require.NotNil(t, coll)
				m := coll.FindForFile("app/pages/index.vue")
				require.NotNil(t, m)
				require.Equal(t, tc.want, Resolve(m, "~/composables/auth"))
				require.Empty(t, Resolve(m, "first/only"), "inherited paths are replaced, not merged")
			}
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

func TestExactAliasPrecedesWildcardWithSamePrefix(t *testing.T) {
	coll := LoadTree(memTree{files: map[string]string{
		"tsconfig.json": `{"compilerOptions":{"paths":{"foo":["exact"],"foo*":["wild/*"]}}}`,
	}})
	scope := coll.FindForFile("caller.ts")
	require.Equal(t, "exact", Resolve(scope, "foo"))
	require.Equal(t, "wild/bar", Resolve(scope, "foobar"))
}

func TestLoad_ExtendsDoesNotLeaveRootThroughSymlink(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "base.json"), []byte(`{"compilerOptions":{"paths":{"~/*":["leaked/*"]}}}`), 0o644))
	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "tsconfig.json"), []byte(`{"extends":"./linked/base.json"}`), 0o644))
	if c := Load(root); c != nil {
		t.Fatalf("extends through a symlink out of the root loaded %+v", c)
	}
}

type countingConfigTree struct {
	memTree
	reads int
}

func (t *countingConfigTree) Read(rel string) ([]byte, bool) {
	t.reads++
	return t.memTree.Read(rel)
}

func TestSharedExtendsVisitsBoundedParents(t *testing.T) {
	files := map[string]string{"tsconfig.json": `{"extends":"./level0.json"}`}
	for i := range 20 {
		files[fmt.Sprintf("level%d.json", i)] = fmt.Sprintf(`{"extends":["./level%d.json","./level%d.json"]}`, i+1, i+1)
	}
	files["level20.json"] = `{"compilerOptions":{"paths":{"~/*":["src/*"]}}}`
	tree := &countingConfigTree{memTree: memTree{files: files}}
	m := LoadTree(tree).FindForFile("app.ts")
	require.Equal(t, "src/auth", Resolve(m, "~/auth"))
	require.Equal(t, len(files), tree.reads)
}
