package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/testenv"
)

// TestScopeForCWD_And_ReposInWorkspace exercises the per-session
// workspace resolution that underpins workspace isolation: a cwd is
// resolved to the workspace/project of the tracked repo that contains
// it, sibling repos sharing a workspace slug resolve together, a repo
// with no declared workspace is its own singleton workspace, and a cwd
// outside every tracked repo fails closed.
func TestScopeForCWD_And_ReposInWorkspace(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")
	repoC := setupRepoDir(t, "repo-c") // no .gortex.yaml → singleton workspace

	// repo-a and repo-b share workspace "alpha"; repo-c declares none.
	require.NoError(t, os.WriteFile(filepath.Join(repoA, ".gortex.yaml"),
		[]byte("workspace: alpha\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoB, ".gortex.yaml"),
		[]byte("workspace: alpha\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
			{Path: repoC, Name: "repo-c"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexScoped("", "") // empty scope → index every configured repo
	require.NoError(t, err)

	// cwd inside repo-a → workspace "alpha", home repo "repo-a".
	ws, _, prefix, ok := mi.ScopeForCWD(repoA)
	require.True(t, ok)
	assert.Equal(t, "alpha", ws)
	assert.Equal(t, "repo-a", prefix)

	// A nested subdirectory of repo-b still resolves to "alpha". The
	// path need not exist on disk — it is the agent's cwd, matched by
	// prefix against the tracked repo root.
	ws, _, prefix, ok = mi.ScopeForCWD(filepath.Join(repoB, "internal", "deep"))
	require.True(t, ok)
	assert.Equal(t, "alpha", ws)
	assert.Equal(t, "repo-b", prefix)

	// repo-c has no declared workspace → singleton workspace keyed on
	// the repo prefix.
	ws, _, prefix, ok = mi.ScopeForCWD(repoC)
	require.True(t, ok)
	assert.Equal(t, "repo-c", ws)
	assert.Equal(t, "repo-c", prefix)

	// A cwd outside every tracked repo must fail closed.
	_, _, _, ok = mi.ScopeForCWD(t.TempDir())
	assert.False(t, ok, "cwd outside every tracked repo must not resolve")

	// An empty cwd fails closed too.
	_, _, _, ok = mi.ScopeForCWD("")
	assert.False(t, ok)

	// ReposInWorkspace("alpha") is exactly {repo-a, repo-b}.
	alpha := mi.ReposInWorkspace("alpha")
	assert.True(t, alpha["repo-a"])
	assert.True(t, alpha["repo-b"])
	assert.False(t, alpha["repo-c"], "repo-c is not in workspace alpha")
	assert.Len(t, alpha, 2)

	// The singleton workspace "repo-c" contains only repo-c.
	singleton := mi.ReposInWorkspace("repo-c")
	assert.Equal(t, map[string]bool{"repo-c": true}, singleton)

	// An unknown workspace resolves to the empty set.
	assert.Empty(t, mi.ReposInWorkspace("does-not-exist"))
}

// TestScopeForCWD_ResolvesCanonicalAliases keeps daemon admission and request
// scoping on one filesystem identity. On macOS the same checkout commonly
// arrives as /tmp/... from a client and /private/tmp/... from Git; an admitted
// session must not become the unresolved, deny-all scope merely because those
// spellings differ.
func TestScopeForCWD_ResolvesCanonicalAliases(t *testing.T) {
	base := t.TempDir()
	realWorkspace := filepath.Join(base, "real-workspace")
	realRepo := filepath.Join(realWorkspace, "repo")
	realNested := filepath.Join(realRepo, "internal")
	require.NoError(t, os.MkdirAll(realNested, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realNested, "main.go"), []byte("package internal\n"), 0o644))
	aliasWorkspace := filepath.Join(base, "workspace-alias")
	if err := os.Symlink(realWorkspace, aliasWorkspace); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasRepo := filepath.Join(aliasWorkspace, "repo")
	aliasNested := filepath.Join(aliasRepo, "internal")

	mi := &MultiIndexer{repos: map[string]*RepoMetadata{
		"repo": {RepoPrefix: "repo", RootPath: realRepo},
	}}
	ws, project, prefix, ok := mi.ScopeForCWD(aliasNested)
	require.True(t, ok)
	assert.Equal(t, "repo", ws)
	assert.Equal(t, "repo", project)
	assert.Equal(t, "repo", prefix)

	repos, workspaces, contained := mi.ContainedReposScope(aliasWorkspace)
	require.True(t, contained)
	assert.Equal(t, []string{"repo"}, repos)
	assert.Equal(t, []string{"repo"}, workspaces)
	assert.Equal(t, "repo", mi.RepoForFile(filepath.Join(aliasNested, "main.go")))
	assert.Equal(t, "repo", mi.RepoForFile(filepath.Join(aliasNested, "new-buffer.go")))
}

func TestScopeForCWD_RefusesAliasToFilesystemRoot(t *testing.T) {
	repo := t.TempDir()
	aliasRoot := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(string(filepath.Separator), aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	mi := &MultiIndexer{repos: map[string]*RepoMetadata{
		"repo": {RepoPrefix: "repo", RootPath: repo},
	}}
	if _, _, _, ok := mi.ScopeForCWD(aliasRoot); ok {
		t.Fatal("a symlink to the filesystem root bypassed ScopeForCWD's broad-root guard")
	}
	if repos, _, ok := mi.ContainedReposScope(aliasRoot); ok || len(repos) != 0 {
		t.Fatalf("a symlink to the filesystem root yielded contained repos: %v", repos)
	}
}

func BenchmarkScopeForCWDAlias(b *testing.B) {
	base := b.TempDir()
	realWorkspace := filepath.Join(base, "real-workspace")
	realRepo := filepath.Join(realWorkspace, "repo")
	realNested := filepath.Join(realRepo, "internal")
	if err := os.MkdirAll(realNested, 0o755); err != nil {
		b.Fatal(err)
	}
	aliasWorkspace := filepath.Join(base, "workspace-alias")
	if err := os.Symlink(realWorkspace, aliasWorkspace); err != nil {
		b.Skipf("symlinks unavailable: %v", err)
	}
	aliasNested := filepath.Join(aliasWorkspace, "repo", "internal")
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{
		"repo": {RepoPrefix: "repo", RootPath: realRepo},
	}}
	for i := range 31 {
		name := fmt.Sprintf("unrelated-%02d", i)
		root := filepath.Join(base, name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			b.Fatal(err)
		}
		mi.repos[name] = &RepoMetadata{RepoPrefix: name, RootPath: root}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, prefix, ok := mi.ScopeForCWD(aliasNested); !ok || prefix != "repo" {
			b.Fatal("alias cwd did not resolve")
		}
	}
}

// TestScopeForCWD_WorkspaceRoot covers the reverse containment: a cwd
// that is not inside any tracked repo but CONTAINS tracked repos (an
// agent session started at a workspace root above its repos). Such a
// cwd binds to the contained repos' workspace when that workspace is
// unambiguous; repos spanning different workspaces keep failing closed
// — a single-workspace session scope cannot express the union.
func TestScopeForCWD_WorkspaceRoot(t *testing.T) {
	mkRepo := func(parent, name string) string {
		t.Helper()
		dir := filepath.Join(parent, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
			[]byte("package main\n\nfunc Hello() {}\n"), 0o644))
		return dir
	}

	// Layout 1: one repo nested two levels under the workspace root.
	parentSingle := t.TempDir()
	app := mkRepo(filepath.Join(parentSingle, "projects"), "app")

	// Layout 2: two repos under one root, sharing workspace "shared".
	parentShared := t.TempDir()
	r1 := mkRepo(parentShared, "r1")
	r2 := mkRepo(parentShared, "r2")
	require.NoError(t, os.WriteFile(filepath.Join(r1, ".gortex.yaml"),
		[]byte("workspace: shared\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(r2, ".gortex.yaml"),
		[]byte("workspace: shared\n"), 0o644))

	// Layout 3: two repos under one root, each its own singleton workspace.
	parentMixed := t.TempDir()
	x := mkRepo(parentMixed, "x")
	y := mkRepo(parentMixed, "y")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: app, Name: "app"},
			{Path: r1, Name: "r1"},
			{Path: r2, Name: "r2"},
			{Path: x, Name: "x"},
			{Path: y, Name: "y"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)

	// Workspace root above exactly one tracked repo: binds with the
	// full scope of that repo — the session behaves as if started
	// inside it (home repo included, so locality ranking still works).
	ws, proj, prefix, ok := mi.ScopeForCWD(parentSingle)
	require.True(t, ok, "workspace root containing one tracked repo must resolve")
	assert.Equal(t, "app", ws)
	assert.Equal(t, "app", proj)
	assert.Equal(t, "app", prefix)

	// Workspace root above two repos that share one workspace slug:
	// binds to the workspace, with no home repo (the session has no
	// single locality anchor).
	ws, proj, prefix, ok = mi.ScopeForCWD(parentShared)
	require.True(t, ok, "workspace root over a single shared workspace must resolve")
	assert.Equal(t, "shared", ws)
	assert.Empty(t, proj, "ambiguous project must stay empty")
	assert.Empty(t, prefix, "no home repo above multiple repos")

	// Workspace root above repos in DIFFERENT workspaces: fail closed —
	// one session scope cannot span two workspace boundaries.
	_, _, _, ok = mi.ScopeForCWD(parentMixed)
	assert.False(t, ok, "mixed-workspace parent must not resolve")

	// The forward direction is untouched: inside a repo still wins.
	ws, _, prefix, ok = mi.ScopeForCWD(filepath.Join(app, "internal"))
	require.True(t, ok)
	assert.Equal(t, "app", ws)
	assert.Equal(t, "app", prefix)
}

// TestContainedReposScope binds a session opened ABOVE its repos to the
// repo set the directory physically contains, with no requirement that
// those repos agree on a workspace slug. Two unrelated repos under one
// parent — each its own workspace by default — is the shape ScopeForCWD
// fails closed on and the shape agents actually open (gortexhq/gortex#418).
//
// The containment answer must stay strictly narrower than any slug: a
// repo that declares a slug the session spans, but sits OUTSIDE the cwd,
// must not appear. That is the property that makes admitting this shape
// safe, so it is asserted directly rather than implied.
func TestContainedReposScope(t *testing.T) {
	mkRepo := func(parent, name string) string {
		t.Helper()
		dir := filepath.Join(parent, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
			[]byte("package main\n\nfunc Hello() {}\n"), 0o644))
		return dir
	}

	// Two unrelated repos under one parent; each is its own workspace.
	parentMixed := t.TempDir()
	x := mkRepo(parentMixed, "x")
	y := mkRepo(parentMixed, "y")

	// A third repo OUTSIDE the parent that declares workspace "x" — the
	// slug one of the contained repos resolves to. Binding by slug would
	// pull it in; binding by containment must not.
	elsewhere := t.TempDir()
	outsider := mkRepo(elsewhere, "outsider")
	require.NoError(t, os.WriteFile(filepath.Join(outsider, ".gortex.yaml"),
		[]byte("workspace: x\n"), 0o644))

	// A parent with no tracked repo under it at all.
	empty := t.TempDir()

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: x, Name: "x"},
			{Path: y, Name: "y"},
			{Path: outsider, Name: "outsider"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)

	// ScopeForCWD still fails closed on the mixed parent — this change
	// adds a second resolution, it does not relax the first.
	_, _, _, ok := mi.ScopeForCWD(parentMixed)
	require.False(t, ok, "ScopeForCWD must keep failing closed on a mixed-workspace parent")

	repos, workspaces, ok := mi.ContainedReposScope(parentMixed)
	require.True(t, ok, "a parent containing tracked repos must resolve")
	assert.Equal(t, []string{"x", "y"}, repos)
	assert.Equal(t, []string{"x", "y"}, workspaces, "undeclared repos are their own workspaces")
	assert.NotContains(t, repos, "outsider",
		"containment must not admit a repo rooted outside the cwd, whatever slug it declares")

	// ReposInWorkspace, the slug-keyed lookup, DOES include the
	// outsider — which is exactly why the session ceiling is the
	// containment set and not the slug's membership.
	assert.True(t, mi.ReposInWorkspace("x")["outsider"],
		"precondition: the outsider shares slug \"x\"")

	// A directory containing nothing tracked stays unresolvable.
	_, _, ok = mi.ContainedReposScope(empty)
	assert.False(t, ok, "a parent with no tracked repo under it must not resolve")

	// A cwd INSIDE a tracked repo contains no repo below it.
	_, _, ok = mi.ContainedReposScope(filepath.Join(x, "internal"))
	assert.False(t, ok, "a directory inside a repo contains no tracked repo")

	// The repo root itself counts as contained (HasPathPrefix is
	// reflexive), so a session opened exactly at a repo root resolves
	// through the forward direction and the reverse one alike.
	repos, _, ok = mi.ContainedReposScope(x)
	require.True(t, ok)
	assert.Equal(t, []string{"x"}, repos)

	// Empty cwd is never a boundary.
	_, _, ok = mi.ContainedReposScope("")
	assert.False(t, ok)
}

// TestContainedReposScope_RefusesOverbroadRoots is the containment rule's
// safety limit. `/` lexically contains every absolute path, so without an
// explicit refusal a session launched at the filesystem root — or at the
// home directory — would bind to the WHOLE graph rather than fail closed.
//
// That is not hypothetical: resolveLaunchCWD (cmd/gortex/proxy.go) falls
// back to exactly these paths when an editor spawns the MCP server with no
// usable working directory, which is the case its own doc comment cites.
// Binding them would convert a missing cwd into maximum scope.
func TestContainedReposScope_RefusesOverbroadRoots(t *testing.T) {
	// Relocate the home directory so the ~/code layout below is built in
	// a sandbox rather than in the developer's real home.
	home := testenv.Sandbox(t).Home
	require.NotEmpty(t, home)

	// Repos under a real parent inside the home directory — the ~/code
	// layout — so home and `/` both lexically contain them.
	parent := filepath.Join(home, "code")
	repoA := filepath.Join(parent, "contained-a")
	repoB := filepath.Join(parent, "contained-b")
	for _, dir := range []string{repoA, repoB} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
			[]byte("package main\n\nfunc Hello() {}\n"), 0o644))
	}

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "contained-a"},
			{Path: repoB, Name: "contained-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)

	// Precondition: a genuine parent inside home still binds, so the
	// refusals below are testing the guard and not an empty repo set.
	repos, _, ok := mi.ContainedReposScope(parent)
	require.True(t, ok, "a real parent-of-repos under home must still bind")
	require.ElementsMatch(t, []string{"contained-a", "contained-b"}, repos)

	for _, cwd := range []string{string(filepath.Separator), home} {
		_, _, ok := mi.ContainedReposScope(cwd)
		assert.False(t, ok, "%q lexically contains every repo and must not bind a session", cwd)

		// ScopeForCWD reaches its own reverse-containment arm first, and
		// sessionScope consults it before ContainedReposScope — so the
		// guard has to hold on both routes or an overbroad root simply
		// takes the other one.
		_, _, _, ok = mi.ScopeForCWD(cwd)
		assert.False(t, ok, "ScopeForCWD must refuse the overbroad root %q too", cwd)
	}
}

// TestScopeForCWD_RefusesOverbroadRootSharedWorkspace is the same guard on
// the route the previous test cannot reach: when every tracked repo shares
// one declared workspace slug, ScopeForCWD's reverse-containment arm
// resolves successfully and never consults ContainedReposScope. Without a
// guard of its own, `/` binds a session to that entire workspace.
func TestScopeForCWD_RefusesOverbroadRootSharedWorkspace(t *testing.T) {
	home := testenv.Sandbox(t).Home
	require.NotEmpty(t, home)

	parent := filepath.Join(home, "code")
	var repos []config.RepoEntry
	for _, name := range []string{"shared-a", "shared-b"} {
		dir := filepath.Join(parent, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"),
			[]byte("workspace: everything\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
			[]byte("package main\n\nfunc Hello() {}\n"), 0o644))
		repos = append(repos, config.RepoEntry{Path: dir, Name: name})
	}

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: repos}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)

	// Precondition: the shared slug makes the reverse-containment arm
	// succeed for a genuine parent, so the refusals below exercise the
	// guard rather than an unrelated failure.
	ws, _, _, ok := mi.ScopeForCWD(parent)
	require.True(t, ok, "a real parent over one shared workspace must still bind")
	require.Equal(t, "everything", ws)

	for _, cwd := range []string{string(filepath.Separator), home} {
		_, _, _, ok := mi.ScopeForCWD(cwd)
		assert.False(t, ok, "%q must not bind to the shared workspace", cwd)
	}
}
