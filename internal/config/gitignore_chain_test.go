package config

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/excludes"
)

// mkGitRepo marks dir as a git repository root by planting a `.git`
// directory — enough for the chain walk, which only tests for presence.
func mkGitRepo(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
}

func mkDir(t *testing.T, parts ...string) string {
	t.Helper()
	dir := filepath.Join(parts...)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	return dir
}

// excludeMatcher builds the matcher the indexer and watcher would use for
// the tracked root, so assertions read as "would this path be indexed".
func excludeMatcher(t *testing.T, cm *ConfigManager, prefix string) *excludes.Matcher {
	t.Helper()
	return excludes.New(cm.EffectiveExclude(prefix))
}

// TestEffectiveExclude_GitignoreAboveTrackedRoot is the issue repro: a
// monorepo whose git root sits above the tracked root. Git applies the
// root `.gitignore` to everything below it, so tracking a subdirectory
// must not silently drop the whole ignore layer.
func TestEffectiveExclude_GitignoreAboveTrackedRoot(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "junk/\n*.log\n")
	app := mkDir(t, root, "projects", "app")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	m := excludeMatcher(t, cm, "r")
	assert.True(t, m.MatchRel("junk/noise.txt"),
		"an unanchored ancestor pattern must apply below the tracked root")
	assert.True(t, m.MatchRel("nested/junk/noise.txt"),
		"an unanchored ancestor pattern matches at any depth, as git does")
	assert.True(t, m.MatchRel("build.log"))
	assert.False(t, m.MatchRel("Real.cs"), "real source must stay indexable")
}

// A .NET-shaped monorepo: the git root's ignore file carries exactly the
// entries the issue reports as missing.
func TestEffectiveExclude_DotNetMonorepoLayout(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "[Oo]bj/\n[Bb]in/\n.vs/\n")
	app := mkDir(t, root, "src", "Solution", "App")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	m := excludeMatcher(t, cm, "r")
	for _, p := range []string{
		"obj/Debug/net8.0/App.AssemblyInfo.cs",
		"obj/project.assets.json",
		"bin/Release/net8.0/App.deps.json",
		".vs/App/v17/HierarchyCache.v1.txt",
	} {
		assert.Truef(t, m.MatchRel(p), "build artifact %q must stay out of the index", p)
	}
	assert.False(t, m.MatchRel("Controllers/HomeController.cs"))
}

// An anchored ancestor pattern only survives if it can consume the whole
// path from its own directory down to the tracked root; what is left is
// re-anchored at the tracked root rather than left matching at any depth.
func TestEffectiveExclude_AncestorAnchoredPatternIsReanchored(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "/projects/app/generated/\n")
	app := mkDir(t, root, "projects", "app")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	m := excludeMatcher(t, cm, "r")
	assert.True(t, m.MatchRel("generated/api.cs"),
		"the anchored ancestor pattern must land on the tracked root")
	assert.False(t, m.MatchRel("src/generated/api.cs"),
		"re-anchoring must stay anchored, not degrade to any-depth matching")
}

// An anchored pattern aimed at a sibling subtree describes nothing inside
// the tracked root and must not leak into its exclude list.
func TestEffectiveExclude_AncestorPatternForSiblingIsDropped(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "/projects/other/\n/tools/gen/\n")
	app := mkDir(t, root, "projects", "app")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	m := excludeMatcher(t, cm, "r")
	assert.False(t, m.MatchRel("other/thing.cs"))
	assert.False(t, m.MatchRel("gen/thing.cs"))
	assert.False(t, m.MatchRel("Real.cs"))
}

// An ancestor pattern that ignores the tracked root itself is dropped: the
// user pointed Gortex at that directory explicitly, and honouring the
// ignore would empty the index of the very repo they asked for.
func TestEffectiveExclude_PatternSwallowingTrackedRootIsDropped(t *testing.T) {
	for _, pattern := range []string{"/projects/app/", "/projects/", "projects/app"} {
		t.Run(pattern, func(t *testing.T) {
			root := t.TempDir()
			mkGitRepo(t, root)
			writeGitignore(t, root, pattern+"\n")
			app := mkDir(t, root, "projects", "app")

			cm := newTestConfigManager(t)
			cm.LoadWorkspaceConfig("r", app)

			m := excludeMatcher(t, cm, "r")
			assert.False(t, m.MatchRel("Real.cs"),
				"an explicitly tracked root must stay indexable")
			assert.False(t, m.MatchRel("src/Real.cs"))
		})
	}
}

// `**` matches zero or more directories, so an ancestor pattern can reach
// into the tracked root at more than one depth. Both readings must survive
// re-anchoring.
func TestEffectiveExclude_AncestorDoubleStarPattern(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "projects/**/artifacts/\n**/coverage/\n")
	app := mkDir(t, root, "projects", "app")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	m := excludeMatcher(t, cm, "r")
	assert.True(t, m.MatchRel("artifacts/gen.cs"), "`**` may consume exactly the tracked root")
	assert.True(t, m.MatchRel("deep/nested/artifacts/gen.cs"), "`**` may also consume more")
	assert.True(t, m.MatchRel("coverage/report.xml"))
	assert.False(t, m.MatchRel("src/App.cs"))
}

// Wildcards in the segments that reach the tracked root are evaluated
// against the real directory names, so a glob that cannot describe this
// tracked root drops out while one that can survives.
func TestEffectiveExclude_AncestorWildcardSegments(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "/*/app/packed/\n/*/other/staged/\n")
	app := mkDir(t, root, "projects", "app")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	m := excludeMatcher(t, cm, "r")
	assert.True(t, m.MatchRel("packed/gen.cs"), "`*` matches the real `projects` segment")
	assert.False(t, m.MatchRel("staged/bundle.js"), "the `other` segment does not match `app`")
}

// Layers are appended outermost first, so the deeper file wins — including
// when it re-includes something an ancestor ignored, which is what git does.
func TestEffectiveExclude_DeeperGitignoreOverridesAncestor(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "*.log\ngenerated/\n")
	app := mkDir(t, root, "projects", "app")
	writeGitignore(t, app, "!keep.log\n!generated/\n")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	m := excludeMatcher(t, cm, "r")
	assert.False(t, m.MatchRel("keep.log"), "the tracked root's re-include must win")
	assert.True(t, m.MatchRel("other.log"), "the ancestor pattern still applies elsewhere")
	assert.False(t, m.MatchRel("generated/api.cs"))
}

// Every `.gitignore` between the git root and the tracked root counts, not
// just the two endpoints.
func TestEffectiveExclude_IntermediateGitignoreApplies(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "root-junk/\n")
	projects := mkDir(t, root, "projects")
	writeGitignore(t, projects, "mid-junk/\n/app/scoped/\n/other/scoped/\n")
	app := mkDir(t, projects, "app")
	writeGitignore(t, app, "leaf-junk/\n")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	m := excludeMatcher(t, cm, "r")
	assert.True(t, m.MatchRel("root-junk/a.txt"))
	assert.True(t, m.MatchRel("mid-junk/a.txt"))
	assert.True(t, m.MatchRel("leaf-junk/a.txt"))
	assert.True(t, m.MatchRel("scoped/a.txt"),
		"an intermediate anchored pattern re-anchors onto the tracked root")
	assert.False(t, m.MatchRel("App.cs"))
}

// Outside a git repository there is no root to layer from, so only the
// tracked root's own `.gitignore` applies — the pre-existing behaviour.
func TestEffectiveExclude_NoGitRootIgnoresAncestors(t *testing.T) {
	root := t.TempDir()
	writeGitignore(t, root, "junk/\n")
	app := mkDir(t, root, "projects", "app")
	writeGitignore(t, app, "own-junk/\n")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	m := excludeMatcher(t, cm, "r")
	assert.True(t, m.MatchRel("own-junk/a.txt"))
	assert.False(t, m.MatchRel("junk/a.txt"),
		"an ancestor outside any repository is not something git would honour")
}

// A nested repository (submodule, vendored clone) is its own root: git does
// not apply the outer repo's ignore file inside it, and neither do we.
func TestEffectiveExclude_NestedRepoStopsAtItsOwnRoot(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "junk/\n")
	inner := mkDir(t, root, "vendor", "inner")
	mkGitRepo(t, inner)
	writeGitignore(t, inner, "inner-junk/\n")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", inner)

	m := excludeMatcher(t, cm, "r")
	assert.True(t, m.MatchRel("inner-junk/a.txt"))
	assert.False(t, m.MatchRel("junk/a.txt"),
		"the outer repo's ignore file does not reach into a nested repository")
}

// A linked worktree or submodule checkout marks its root with a `.git`
// *file*, not a directory. Presence is what matters, not the type.
func TestEffectiveExclude_GitFileMarksRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git"),
		[]byte("gitdir: /elsewhere/.git/worktrees/wt\n"), 0o644))
	writeGitignore(t, root, "junk/\n")
	app := mkDir(t, root, "projects", "app")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	assert.True(t, excludeMatcher(t, cm, "r").MatchRel("junk/a.txt"))
}

// `respect_gitignore: false` opts out of the whole layer, ancestors included.
func TestEffectiveExclude_RespectGitignoreFalseSkipsChain(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "junk/\n")
	app := mkDir(t, root, "projects", "app")
	writeGitignore(t, app, "own-junk/\n")
	writeWorkspace(t, app, "respect_gitignore: false\n")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	m := excludeMatcher(t, cm, "r")
	assert.False(t, m.MatchRel("junk/a.txt"))
	assert.False(t, m.MatchRel("own-junk/a.txt"))
}

// The cache key spans the whole chain, so editing an ancestor file — not
// just the tracked root's own — invalidates the merged list, and exactly
// the changed file is re-read.
func TestEffectiveExclude_AncestorGitignoreEditInvalidatesCache(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "old-only/\n")
	app := mkDir(t, root, "projects", "app")
	writeGitignore(t, app, "leaf/\n")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)
	reads := countingReader(cm)

	first := excludes.New(cm.EffectiveExclude("r"))
	assert.True(t, first.MatchRel("old-only/a.txt"))
	require.Equal(t, int64(2), atomic.LoadInt64(reads), "both chain members read once")

	_ = cm.EffectiveExclude("r")
	require.Equal(t, int64(2), atomic.LoadInt64(reads), "the second call must be a cache hit")

	writeGitignore(t, root, "new-only/\nsecond-new/\n")
	bumpMtime(t, filepath.Join(root, ".gitignore"))

	second := excludes.New(cm.EffectiveExclude("r"))
	assert.True(t, second.MatchRel("new-only/a.txt"), "the ancestor edit must take effect")
	assert.False(t, second.MatchRel("old-only/a.txt"), "stale ancestor patterns must be dropped")
	assert.True(t, second.MatchRel("leaf/a.txt"), "the unchanged leaf layer survives")
	assert.Equal(t, int64(3), atomic.LoadInt64(reads),
		"only the changed ancestor is re-read; the leaf stays cached")
}

// A `.gitignore` appearing mid-session in a directory that had none changes
// the shape of the chain, which the merged cache must notice.
func TestEffectiveExclude_NewIntermediateGitignoreInvalidatesCache(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "root-junk/\n")
	projects := mkDir(t, root, "projects")
	app := mkDir(t, projects, "app")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)

	first := excludeMatcher(t, cm, "r")
	assert.True(t, first.MatchRel("root-junk/a.txt"))
	assert.False(t, first.MatchRel("mid-junk/a.txt"))

	writeGitignore(t, projects, "mid-junk/\n")

	second := excludeMatcher(t, cm, "r")
	assert.True(t, second.MatchRel("mid-junk/a.txt"),
		"a newly added intermediate .gitignore must invalidate the cached merge")
	assert.True(t, second.MatchRel("root-junk/a.txt"))
}

// Two tracked roots under one git root see the same ancestor layer, each
// re-anchored to its own depth.
func TestEffectiveExclude_SiblingTrackedRootsShareAncestor(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "junk/\n/projects/a/gen/\n/projects/b/gen/\n")
	a := mkDir(t, root, "projects", "a")
	b := mkDir(t, root, "projects", "b")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("a", a)
	cm.LoadWorkspaceConfig("b", b)

	for _, prefix := range []string{"a", "b"} {
		m := excludeMatcher(t, cm, prefix)
		assert.Truef(t, m.MatchRel("junk/x.txt"), "prefix %q", prefix)
		assert.Truef(t, m.MatchRel("gen/x.cs"), "prefix %q", prefix)
		assert.Falsef(t, m.MatchRel("src/x.cs"), "prefix %q", prefix)
	}
}

// The chain's shape is memoized per tracked root, so a `.git` appearing
// above it mid-session is picked up when the repo is next published —
// tracked, reindexed, or refreshed — rather than on every call.
func TestEffectiveExclude_RepublishRediscoversGitRoot(t *testing.T) {
	root := t.TempDir()
	writeGitignore(t, root, "junk/\n")
	app := mkDir(t, root, "projects", "app")

	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", app)
	assert.False(t, excludeMatcher(t, cm, "r").MatchRel("junk/a.txt"),
		"no repository yet, so the ancestor file does not apply")

	mkGitRepo(t, root)
	cm.LoadWorkspaceConfig("r", app)

	assert.True(t, excludeMatcher(t, cm, "r").MatchRel("junk/a.txt"),
		"republishing the repo must rediscover its git root")
}

func TestGitignoreChain_TrackedRootIsGitRoot(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	writeGitignore(t, root, "junk/\n")

	chain := gitignoreChain(root)
	require.Len(t, chain, 1, "the git root's own file is the whole chain")
	assert.Equal(t, root, chain[0].dir)
	assert.Empty(t, chain[0].sub)
	assert.True(t, chain[0].stat.exists)
}

func TestGitignoreChain_OrderedOutermostFirst(t *testing.T) {
	root := t.TempDir()
	mkGitRepo(t, root)
	projects := mkDir(t, root, "projects")
	app := mkDir(t, projects, "app")

	chain := gitignoreChain(app)
	require.Len(t, chain, 3)
	assert.Equal(t, []string{root, projects, app},
		[]string{chain[0].dir, chain[1].dir, chain[2].dir})
	assert.Equal(t, []string{"projects/app", "app", ""},
		[]string{chain[0].sub, chain[1].sub, chain[2].sub})
}

func TestReanchorPattern(t *testing.T) {
	sub := []string{"projects", "app"}
	cases := []struct {
		name    string
		pattern string
		want    []string
	}{
		{"unanchored carries over", "obj/", []string{"obj/"}},
		{"unanchored glob carries over", "*.log", []string{"*.log"}},
		{"unanchored negation carries over", "!keep.log", []string{"!keep.log"}},
		{"anchored on path re-anchors", "/projects/app/obj/", []string{"/obj/"}},
		{"anchored without leading slash re-anchors", "projects/app/obj", []string{"/obj"}},
		{"anchored negation keeps its bang", "!/projects/app/obj/", []string{"!/obj/"}},
		{"deeper tail survives", "/projects/app/a/b/c", []string{"/a/b/c"}},
		{"sibling subtree drops out", "/projects/other/obj/", nil},
		{"unrelated root entry drops out", "/build/", nil},
		{"pattern swallowing the tracked root drops out", "/projects/app/", nil},
		{"pattern swallowing an ancestor drops out", "/projects/", nil},
		{"wildcard segment matches the real name", "/*/app/obj/", []string{"/obj/"}},
		{"wildcard segment that misses drops out", "/*/other/obj/", nil},
		{"class segment matches the real name", "/[Pp]rojects/app/obj/", []string{"/obj/"}},
		{"double star spans the tracked root", "projects/**/obj/", []string{"**/obj/"}},
		{"leading double star stays depth-agnostic", "**/obj/", []string{"**/obj/"}},
		{"trailing double star re-anchors", "/projects/app/gen/**", []string{"/gen/**"}},
		{"comment drops out", "# comment", nil},
		{"blank drops out", "   ", nil},
		{"bare slash drops out", "/", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, reanchorPattern(tc.pattern, sub))
		})
	}
}

func TestReanchorGitignore_EmptySubIsIdentity(t *testing.T) {
	in := []string{"/build/", "*.log", "a/b/c"}
	assert.Equal(t, in, reanchorGitignore(in, ""))
	assert.Equal(t, in, reanchorGitignore(in, "."))
}

// A pattern packed with `**` must stay linear-ish rather than branching
// exponentially over the chain depth.
func TestAnchoredRemainders_DoubleStarDoesNotExplode(t *testing.T) {
	pat := make([]string, 0, 41)
	for i := 0; i < 20; i++ {
		pat = append(pat, "**", "x")
	}
	pat = append(pat, "tail")
	sub := make([]string, 20)
	for i := range sub {
		sub[i] = "x"
	}
	assert.NotEmpty(t, anchoredRemainders(pat, sub))
}

func TestEffectiveExcludeForRootReplacesCanonicalGitignore(t *testing.T) {
	canonical := t.TempDir()
	selected := t.TempDir()
	mkGitRepo(t, canonical)
	mkGitRepo(t, selected)
	writeGitignore(t, canonical, "old.go\n")
	writeGitignore(t, selected, "new.go\n")
	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("r", canonical)
	ordinary := excludes.New(cm.EffectiveExclude("r"))
	require.True(t, ordinary.MatchRel("old.go"))
	alternate := excludes.New(cm.EffectiveExcludeForRoot("r", selected))
	require.False(t, alternate.MatchRel("old.go"), "selected checkout must not inherit canonical gitignore")
	require.True(t, alternate.MatchRel("new.go"))
	info, err := os.Stat(filepath.Join(selected, ".gitignore"))
	require.NoError(t, err)
	writeGitignore(t, selected, "old.go\n")
	require.NoError(t, os.Chtimes(filepath.Join(selected, ".gitignore"), info.ModTime(), info.ModTime()))
	alternate = excludes.New(cm.EffectiveExcludeForRoot("r", selected))
	require.True(t, alternate.MatchRel("old.go"), "scoped source policy must read same-size restored-mtime edits")
	require.False(t, alternate.MatchRel("new.go"))
	ordinary = excludes.New(cm.EffectiveExclude("r"))
	require.True(t, ordinary.MatchRel("old.go"), "alternate cache must not poison canonical root")
	require.False(t, ordinary.MatchRel("new.go"))
}
