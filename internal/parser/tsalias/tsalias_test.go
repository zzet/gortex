package tsalias

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolve_WildcardAlias(t *testing.T) {
	m := &Map{
		Entries: []Alias{
			{AliasPrefix: "@/", TargetPrefix: "src/", HasWildcard: true},
		},
	}
	assert.Equal(t, "src/lib/foo", Resolve(m, "@/lib/foo"))
}

func TestResolve_ExactAlias(t *testing.T) {
	m := &Map{
		Entries: []Alias{
			{AliasPrefix: "$utils", TargetPrefix: "src/util/index.ts"},
		},
	}
	assert.Equal(t, "src/util/index", Resolve(m, "$utils"), "extension stripped")
	assert.Equal(t, "", Resolve(m, "$utils/format"), "exact match doesn't fire on a longer specifier")
}

func TestResolve_BaseURLPrepended(t *testing.T) {
	m := &Map{
		BaseURL: "src",
		Entries: []Alias{
			{AliasPrefix: "@/", TargetPrefix: "", HasWildcard: true},
		},
	}
	assert.Equal(t, "src/lib/foo", Resolve(m, "@/lib/foo"))
}

func TestResolve_LongestPrefixWins(t *testing.T) {
	m := &Map{
		Entries: []Alias{
			{AliasPrefix: "@components/", TargetPrefix: "src/components/", HasWildcard: true},
			{AliasPrefix: "@/", TargetPrefix: "src/", HasWildcard: true},
		},
	}
	// Caller sorts entries; assert that with the sorted order the
	// longer prefix matches.
	assert.Equal(t, "src/components/Button", Resolve(m, "@components/Button"))
	assert.Equal(t, "src/Button", Resolve(m, "@/Button"))
}

func TestResolve_NoMatch(t *testing.T) {
	m := &Map{
		Entries: []Alias{
			{AliasPrefix: "@/", TargetPrefix: "src/", HasWildcard: true},
		},
	}
	assert.Equal(t, "", Resolve(m, "react"))
	assert.Equal(t, "", Resolve(nil, "@/foo"))
}

func TestLoad_FindsTsconfigAtRoot(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
  "compilerOptions": {
    "baseUrl": ".",
    "paths": {
      "@/*": ["src/*"],
      "@app": ["src/app/index.ts"]
    }
  }
}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(cfg), 0644))

	coll := Load(dir)
	require.NotNil(t, coll)
	require.Len(t, coll.Maps(), 1)

	m := coll.FindForFile("src/components/Button.tsx")
	require.NotNil(t, m)
	assert.Equal(t, "src/foo", Resolve(m, "@/foo"))
	assert.Equal(t, "src/app/index", Resolve(m, "@app"))
}

func TestLoad_NestedTsconfigWinsForChildren(t *testing.T) {
	dir := t.TempDir()
	rootCfg := `{
  "compilerOptions": {
    "paths": { "@/*": ["root/*"] }
  }
}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(rootCfg), 0644))

	nestedDir := filepath.Join(dir, "packages", "web")
	require.NoError(t, os.MkdirAll(nestedDir, 0755))
	nestedCfg := `{
  "compilerOptions": {
    "paths": { "@/*": ["nested/*"] }
  }
}`
	require.NoError(t, os.WriteFile(filepath.Join(nestedDir, "tsconfig.json"), []byte(nestedCfg), 0644))

	coll := Load(dir)
	require.NotNil(t, coll)

	// A file under packages/web/ picks up the nested map.
	m := coll.FindForFile("packages/web/src/App.tsx")
	require.NotNil(t, m)
	assert.Equal(t, "packages/web/nested/foo", Resolve(m, "@/foo"),
		"nested config target is rooted at the config's own directory")

	// A file outside packages/web/ falls back to the root map.
	m = coll.FindForFile("apps/api/src/server.ts")
	require.NotNil(t, m)
	assert.Equal(t, "root/foo", Resolve(m, "@/foo"))
}

func TestLoad_SkipsNodeModules(t *testing.T) {
	dir := t.TempDir()
	nm := filepath.Join(dir, "node_modules", "some-pkg")
	require.NoError(t, os.MkdirAll(nm, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(nm, "tsconfig.json"),
		[]byte(`{"compilerOptions":{"paths":{"@/*":["should/not/be/used/*"]}}}`), 0644))

	coll := Load(dir)
	// Either nil (no usable configs found) or a collection with zero
	// entries — both prove the node_modules config was skipped.
	if coll == nil {
		return
	}
	for _, m := range coll.Maps() {
		assert.NotContains(t, m.DirPrefix, "node_modules")
	}
}

func TestLoad_NoConfigsReturnsNil(t *testing.T) {
	dir := t.TempDir()
	assert.Nil(t, Load(dir))
}

func TestLoad_MalformedConfigSkipped(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tsconfig.json"),
		[]byte(`{ this is not json`), 0644))
	// Should not panic; should return nil because no usable config.
	assert.Nil(t, Load(dir))
}

func tsaliasWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestJSONCTolerantAliasResolution verifies a tsconfig using // and /* */
// comments and trailing commas still yields its aliases (instead of dropping
// every alias for the repo), and that a multi-target alias resolves to the
// target that actually exists on disk rather than blindly to the first.
func TestJSONCTolerantAliasResolution(t *testing.T) {
	root := t.TempDir()
	cfg := `{
  // path aliases for the app
  "compilerOptions": {
    "baseUrl": ".",
    "paths": {
      /* first target does not exist; second does */
      "@/*": ["nonexistent/*", "src/*"],
      "$config": ["src/config.ts"],
    },
  },
}`
	tsaliasWrite(t, filepath.Join(root, "tsconfig.json"), cfg)
	tsaliasWrite(t, filepath.Join(root, "src", "Button.ts"), "export const Button = 1;")
	tsaliasWrite(t, filepath.Join(root, "src", "config.ts"), "export const c = 1;")

	col := Load(root)
	require.NotNil(t, col, "JSONC tsconfig must still produce a collection")
	m := col.FindForFile("app.ts")
	require.NotNil(t, m)
	require.NotEmpty(t, m.Entries, "comments / trailing commas must not wipe the aliases")

	// Disk-grounded multi-target: @/Button resolves to the existing src/Button,
	// not the first (nonexistent) target.
	assert.Equal(t, "src/Button", Resolve(m, "@/Button"))
	assert.Equal(t, "src/config", Resolve(m, "$config"))

	// An import with no matching alias still resolves to "".
	assert.Equal(t, "", Resolve(m, "react"))
}

// TestStripJSONCPreservesStrings ensures the stripper never touches
// comment-like or comma sequences inside string literals.
func TestStripJSONCPreservesStrings(t *testing.T) {
	in := []byte(`{"a": "http://x.y/* z", "b": "trailing,", /* c */ "d": 1,}`)
	out := string(stripJSONC(in))
	if !contains(out, `"http://x.y/* z"`) {
		t.Fatalf("string content was mangled: %s", out)
	}
	if !contains(out, `"trailing,"`) {
		t.Fatalf("comma inside string was stripped: %s", out)
	}
	if contains(out, "/* c */") {
		t.Fatalf("block comment survived: %s", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// memTree is a Tree with no filesystem behind it — the shape a committed
// snapshot presents: every path it holds, offered in one enumeration, with no
// directories of its own to prune.
type memTree struct{ files map[string]string }

func (t memTree) Files(visit func(rel string)) {
	names := make([]string, 0, len(t.files))
	for rel := range t.files {
		names = append(names, rel)
	}
	sort.Strings(names)
	for _, rel := range names {
		visit(rel)
	}
}

func (t memTree) Read(rel string) ([]byte, bool) {
	body, ok := t.files[rel]
	return []byte(body), ok
}

func (t memTree) Exists(rel string) bool {
	_, ok := t.files[rel]
	return ok
}

// tsaliasTreeFixture is one repository layout, as content: a root config, a
// nested config, a config inside node_modules that must never become a scope,
// and a multi-target alias whose FIRST target does not exist.
func tsaliasTreeFixture() map[string]string {
	return map[string]string{
		"tsconfig.json": `{"compilerOptions":{"baseUrl":".","paths":` +
			`{"@x":["src/x.ts"],"@multi/*":["absent/*","src/*"]}}}`,
		"packages/web/tsconfig.json":     `{"compilerOptions":{"paths":{"@x":["nested/x.ts"]}}}`,
		"node_modules/dep/tsconfig.json": `{"compilerOptions":{"paths":{"@x":["never/used.ts"]}}}`,
		"src/x.ts":                       "export const x = 1;\n",
		"packages/web/nested/x.ts":       "export const x = 2;\n",
	}
}

// TestLoadTree_ReadsATreeWithNoFilesystem pins the loader a committed build
// uses: the scopes, their order, the skip rule and the multi-target probe all
// come from the tree's own content, with nothing on disk to fall back to.
func TestLoadTree_ReadsATreeWithNoFilesystem(t *testing.T) {
	coll := LoadTree(memTree{files: tsaliasTreeFixture()})
	require.NotNil(t, coll)
	require.Len(t, coll.Maps(), 2, "the node_modules config is not a scope")

	root := coll.FindForFile("src/app.ts")
	require.NotNil(t, root)
	assert.Equal(t, "src/x", Resolve(root, "@x"))
	assert.Equal(t, "src/x", Resolve(root, "@multi/x"),
		"a multi-target alias is grounded in the tree, not on disk")

	nested := coll.FindForFile("packages/web/src/app.ts")
	require.NotNil(t, nested)
	assert.Equal(t, "packages/web/nested/x", Resolve(nested, "@x"),
		"the nearest-ancestor scope wins, rooted at its own directory")
}

// TestLoadTree_AgreesWithLoadOverTheSameContent is the equivalence the routing
// rests on: one layout, read as a working copy and as a tree, yields the same
// answers. Load is LoadTree over the working copy, so a divergence here would
// mean a committed build and the live index disagree about a repository whose
// bytes are identical.
func TestLoadTree_AgreesWithLoadOverTheSameContent(t *testing.T) {
	files := tsaliasTreeFixture()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}

	disk, mem := Load(root), LoadTree(memTree{files: files})
	require.NotNil(t, disk)
	require.NotNil(t, mem)
	require.Equal(t, len(disk.Maps()), len(mem.Maps()))
	for i := range disk.Maps() {
		assert.Equal(t, disk.Maps()[i].DirPrefix, mem.Maps()[i].DirPrefix)
	}
	for _, probe := range []struct{ file, spec string }{
		{"src/app.ts", "@x"},
		{"src/app.ts", "@multi/x"},
		{"packages/web/src/app.ts", "@x"},
		{"node_modules/dep/app.ts", "@x"},
		{"src/app.ts", "react"},
	} {
		assert.Equal(t,
			Resolve(disk.FindForFile(path.Dir(probe.file)), probe.spec),
			Resolve(mem.FindForFile(path.Dir(probe.file)), probe.spec),
			"%s / %s", probe.file, probe.spec)
	}
}

// TestNewCollection_OrdersNearestAncestorFirst pins the constructor's one
// contract beyond "hold these maps": FindForFile is a single linear scan, so
// the deepest scope has to come first however the caller assembled the set.
func TestNewCollection_OrdersNearestAncestorFirst(t *testing.T) {
	assert.Nil(t, NewCollection(nil))
	assert.Nil(t, NewCollection([]*Map{nil}))

	coll := NewCollection([]*Map{{DirPrefix: ""}, {DirPrefix: "a/b/c"}, {DirPrefix: "a"}})
	require.NotNil(t, coll)
	require.Len(t, coll.Maps(), 3)
	assert.Equal(t, "a/b/c", coll.Maps()[0].DirPrefix)
	assert.Equal(t, "a", coll.Maps()[1].DirPrefix)
	assert.Equal(t, "", coll.Maps()[2].DirPrefix)
	assert.Equal(t, "a/b/c", coll.FindForFile("a/b/c/d").DirPrefix)
	assert.Equal(t, "a", coll.FindForFile("a/z").DirPrefix)
	assert.Equal(t, "", coll.FindForFile("q").DirPrefix)
}
