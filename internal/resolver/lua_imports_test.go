package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/graph"
)

func luaRequire(g graph.Store, from, target string, roblox string) *graph.Edge {
	e := &graph.Edge{From: from, To: "unresolved::import::" + target, Kind: graph.EdgeImports}
	if roblox != "" {
		e.Meta = map[string]any{"roblox_path": roblox}
	}
	g.AddEdge(e)
	return e
}

// TestResolveLuaRequires_StringModule pins the classic dotted require:
// require("mod.a.b.c") binds to the repo-relative mod/a/b/c.lua file.
func TestResolveLuaRequires_StringModule(t *testing.T) {
	g := graph.New()
	seedFile(g, "main.lua", "lua")
	seedFile(g, "mod/a/b/c.lua", "lua")
	e := luaRequire(g, "main.lua", "mod.a.b.c", "")

	r := New(g)
	r.resolveLuaRequires()

	assert.Equal(t, "mod/a/b/c.lua", e.To)
	assert.Equal(t, graph.OriginASTResolved, e.Origin)
}

// TestResolveLuaRequires_InitModule pins that a require for a package directory
// binds to that directory's init.lua.
func TestResolveLuaRequires_InitModule(t *testing.T) {
	g := graph.New()
	seedFile(g, "main.lua", "lua")
	seedFile(g, "mod/a/b/init.lua", "lua")
	e := luaRequire(g, "main.lua", "mod.a.b", "")

	r := New(g)
	r.resolveLuaRequires()

	assert.Equal(t, "mod/a/b/init.lua", e.To, "dir module resolves to init.lua")
}

// TestResolveLuaRequires_LuauExtension pins resolution onto a .luau file.
func TestResolveLuaRequires_LuauExtension(t *testing.T) {
	g := graph.New()
	seedFile(g, "main.luau", "luau")
	seedFile(g, "shared/util.luau", "luau")
	e := luaRequire(g, "main.luau", "shared.util", "")

	r := New(g)
	r.resolveLuaRequires()

	assert.Equal(t, "shared/util.luau", e.To)
}

// TestResolveLuaRequires_PackageRootSuffix pins the package-root net: a dotted
// require resolves to a uniquely-matching file under a package root.
func TestResolveLuaRequires_PackageRootSuffix(t *testing.T) {
	g := graph.New()
	seedFile(g, "main.lua", "lua")
	seedFile(g, "src/a/b/c.lua", "lua") // under a package root, not at a/b/c.lua
	e := luaRequire(g, "main.lua", "a.b.c", "")

	r := New(g)
	r.resolveLuaRequires()

	assert.Equal(t, "src/a/b/c.lua", e.To, "dotted require resolves under a package root")
}

// TestResolveLuaRequires_RobloxUnique pins a Roblox instance-path require
// binding by its unique leaf module file.
func TestResolveLuaRequires_RobloxUnique(t *testing.T) {
	g := graph.New()
	seedFile(g, "main.luau", "luau")
	seedFile(g, "src/components/Foo.luau", "luau")
	e := luaRequire(g, "main.luau", "Foo", "script.Parent.Foo")

	r := New(g)
	r.resolveLuaRequires()

	assert.Equal(t, "src/components/Foo.luau", e.To)
}

// TestResolveLuaRequires_RobloxAmbiguousStaysExternal pins that an ambiguous
// Roblox module name does not bind.
func TestResolveLuaRequires_RobloxAmbiguousStaysExternal(t *testing.T) {
	g := graph.New()
	seedFile(g, "main.luau", "luau")
	seedFile(g, "a/Foo.luau", "luau")
	seedFile(g, "b/Foo.luau", "luau")
	e := luaRequire(g, "main.luau", "Foo", "script.Parent.Foo")

	r := New(g)
	r.resolveLuaRequires()

	assert.Equal(t, "unresolved::import::Foo", e.To, "ambiguous Roblox module stays external")
}

// TestResolveLuaRequires_UnindexedStaysExternal pins that a require with no
// indexed target is left external.
func TestResolveLuaRequires_UnindexedStaysExternal(t *testing.T) {
	g := graph.New()
	seedFile(g, "main.lua", "lua")
	e := luaRequire(g, "main.lua", "nonexistent.module", "")

	r := New(g)
	r.resolveLuaRequires()

	assert.Equal(t, "unresolved::import::nonexistent.module", e.To)
}

// luauTree seeds a Rojo-style Luau tree that exercises every require-by-string
// base: a plain module, an init module, a module with children and a
// name that exists only in another folder.
func luauTree(g graph.Store) {
	for _, f := range []string{
		"top.luau",
		"src/shared/MatchState.luau",
		"src/shared/MatchState/Child.luau",
		"src/shared/Economy.luau",
		"src/shared/Pkg/init.luau",
		"src/shared/Pkg/Inner.luau",
		"src/shared/Both.luau",
		"src/shared/Both/init.luau",
		"src/other/Util.luau",
	} {
		seedFile(g, f, "luau")
	}
}

// TestResolveLuaRequires_LuauPaths pins Luau's require-by-string rules: `./`
// and `../` start at the requiring file's directory, an init file stands in
// for its directory, `@self` is the module itself, and a path that names no
// single file stays unresolved rather than binding elsewhere.
func TestResolveLuaRequires_LuauPaths(t *testing.T) {
	cases := []struct {
		name, from, spec, want string
	}{
		{"sibling", "src/shared/MatchState.luau", "./Economy", "src/shared/Economy.luau"},
		{"parent", "src/shared/MatchState.luau", "../other/Util", "src/other/Util.luau"},
		{"directory module", "src/shared/MatchState.luau", "./Pkg", "src/shared/Pkg/init.luau"},
		{"self child", "src/shared/MatchState.luau", "@self/Child", "src/shared/MatchState/Child.luau"},
		{"init self", "src/shared/Pkg/init.luau", "@self/Inner", "src/shared/Pkg/Inner.luau"},
		{"init sibling is its folder's sibling", "src/shared/Pkg/init.luau", "./Economy", "src/shared/Economy.luau"},
		{"missing stays unresolved", "src/shared/MatchState.luau", "./Missing", ""},
		{"no suffix fallback", "src/other/Util.luau", "./Economy", ""},
		{"file and init module collide", "src/shared/MatchState.luau", "./Both", ""},
		{"climbs out of the root", "top.luau", "../Economy", ""},
		{"config alias not modelled", "src/shared/MatchState.luau", "@game/ReplicatedStorage/Shared/Economy", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			luauTree(g)
			e := luaRequire(g, tc.from, tc.spec, "")

			New(g).resolveLuaRequires()

			want := tc.want
			if want == "" {
				want = "unresolved::import::" + tc.spec
			}
			assert.Equal(t, want, e.To)
		})
	}
}

// TestResolveAll_LuauRequiresReachLuaPass catches the generic import resolver
// claiming Lua requires first: it stubbed `./Economy` to `external::` and bound
// `./Pkg` to whichever file sat in a `Pkg/` folder, so the Lua pass never ran.
func TestResolveAll_LuauRequiresReachLuaPass(t *testing.T) {
	g := graph.New()
	luauTree(g)
	from := "src/shared/MatchState.luau"
	sibling := &graph.Edge{From: from, To: "unresolved::import::./Economy", Kind: graph.EdgeImports, FilePath: from, Line: 2}
	dirModule := &graph.Edge{From: from, To: "unresolved::import::./Pkg", Kind: graph.EdgeImports, FilePath: from, Line: 3}
	g.AddEdge(sibling)
	g.AddEdge(dirModule)

	New(g).ResolveAll()

	assert.Equal(t, "src/shared/Economy.luau", sibling.To)
	assert.Equal(t, "src/shared/Pkg/init.luau", dirModule.To)
}
