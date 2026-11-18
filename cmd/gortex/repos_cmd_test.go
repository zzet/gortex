package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// gitInitRepo creates a real git repository at dir with one commit and
// returns its HEAD SHA. A real repo (not a bare .git dir) is required
// because runRepos shells out to `git rev-parse HEAD`.
func gitInitRepo(t *testing.T, dir string) (headSHA string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Keep the commit deterministic and independent of the
		// developer's global git identity.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	run("init")
	run("checkout", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644))
	run("add", "README.md")
	run("commit", "-m", "initial")
	return gitCommitHash(dir)
}

// gitCommitMore stages a new file and commits it, advancing HEAD.
// Returns the new HEAD SHA.
func gitCommitMore(t *testing.T, dir, file string) (headSHA string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte("y"), 0o644))
	run("add", file)
	run("commit", "-m", "more")
	return gitCommitHash(dir)
}

// reposTestEnv writes a temp global config tracking the given repo
// entries, points the package-level cfgFile at it, and routes the
// freshness store the `repos` command reads at an isolated temp dir.
// Both package globals are restored on cleanup.
func reposTestEnv(t *testing.T, repos []config.RepoEntry) {
	t.Helper()
	root := t.TempDir()

	gc := &config.GlobalConfig{Repos: repos}
	gcPath := filepath.Join(root, "config.yaml")
	gc.SetConfigPath(gcPath)
	require.NoError(t, gc.Save())

	prevCfg := cfgFile
	cfgFile = gcPath
	prevBackend := reposBackendPath
	// Isolate the SQLite freshness store at a per-test path so the
	// command never reads the developer's real ~/.gortex/store. Tests
	// that exercise the repo_index_state path seed this file; the rest
	// leave it absent so every repo reports as never indexed.
	reposBackendPath = filepath.Join(root, "store", "store.sqlite")
	t.Cleanup(func() {
		cfgFile = prevCfg
		reposBackendPath = prevBackend
	})
}

// seedIndexState writes a repo_index_state freshness row into the test's
// isolated SQLite backend store — the same table the daemon writes when it tracks or warms up
// a repo, and the authoritative source describeRepo reads first.
func seedIndexState(t *testing.T, prefix, sha string, dirty bool, indexedAt time.Time) {
	t.Helper()
	seedIndexStateAt(t, reposBackendPath, prefix, sha, dirty, indexedAt)
}

// seedIndexStateAt writes a freshness row into an explicitly named store, for
// the tests that care about WHICH store the command decided to read.
func seedIndexStateAt(t *testing.T, path, prefix, sha string, dirty bool, indexedAt time.Time) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	st, err := store_sqlite.Open(path)
	require.NoError(t, err)
	require.NoError(t, st.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: prefix,
		IndexedSHA: sha,
		Dirty:      dirty,
		IndexedAt:  indexedAt.Unix(),
	}))
	require.NoError(t, st.Close())
}

func newReposCmd() (*cobra.Command, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	c := &cobra.Command{}
	c.SetOut(buf)
	c.SetErr(buf)
	return c, buf
}

// TestRunRepos_JSON_FreshAndStale covers the JSON shape, the head-commit
// field, and the freshness/staleness flag across the three states: a
// repo whose index matches HEAD (fresh), one whose HEAD has advanced
// past the indexed commit (stale), and one never indexed.
func TestRunRepos_JSON_FreshAndStale(t *testing.T) {
	base := t.TempDir()
	freshDir := filepath.Join(base, "fresh-repo")
	staleDir := filepath.Join(base, "stale-repo")
	neverDir := filepath.Join(base, "never-repo")

	freshHead := gitInitRepo(t, freshDir)
	oldHead := gitInitRepo(t, staleDir)
	neverHead := gitInitRepo(t, neverDir)

	reposTestEnv(t, []config.RepoEntry{
		{Path: freshDir, Name: "fresh-repo", Workspace: "ws-fresh"},
		{Path: staleDir, Name: "stale-repo"},
		{Path: neverDir, Name: "never-repo"},
	})

	indexedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	// fresh-repo: indexed at the exact current HEAD.
	seedIndexState(t, "fresh-repo", freshHead, false, indexedAt)
	// stale-repo: indexed at the old HEAD, then advance HEAD.
	seedIndexState(t, "stale-repo", oldHead, false, indexedAt)
	newHead := gitCommitMore(t, staleDir, "second.txt")
	require.NotEqual(t, oldHead, newHead)
	// never-repo: no freshness row seeded.

	reposJSON = true
	t.Cleanup(func() { reposJSON = false })

	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))

	var got []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 3)

	// Output is sorted by name: fresh, never, stale.
	byName := map[string]repoStatus{}
	for _, e := range got {
		byName[e.Name] = e
	}

	fresh := byName["fresh-repo"]
	assert.Equal(t, freshHead, fresh.HeadCommit, "head commit must be the current git HEAD")
	assert.Equal(t, freshHead, fresh.IndexedCommit)
	assert.Equal(t, "main", fresh.Branch)
	assert.Equal(t, "ws-fresh", fresh.Workspace)
	assert.True(t, fresh.Indexed)
	assert.False(t, fresh.Stale, "index matches HEAD → not stale")
	require.NotNil(t, fresh.LastIndexed)
	assert.Equal(t, indexedAt.Unix(), fresh.LastIndexed.Unix())

	stale := byName["stale-repo"]
	assert.Equal(t, newHead, stale.HeadCommit)
	assert.Equal(t, oldHead, stale.IndexedCommit)
	assert.True(t, stale.Indexed)
	assert.True(t, stale.Stale, "HEAD advanced past the indexed commit → stale")
	require.NotNil(t, stale.LastIndexed)

	never := byName["never-repo"]
	assert.Equal(t, neverHead, never.HeadCommit, "head commit reported even without an index")
	assert.Empty(t, never.IndexedCommit)
	assert.False(t, never.Indexed)
	assert.True(t, never.Stale, "never indexed → stale")
	assert.Nil(t, never.LastIndexed)
}

// TestRunRepos_Table renders the default (non-JSON) form and asserts
// the freshness vocabulary appears for each state.
func TestRunRepos_Table(t *testing.T) {
	base := t.TempDir()
	freshDir := filepath.Join(base, "alpha")
	neverDir := filepath.Join(base, "beta")
	freshHead := gitInitRepo(t, freshDir)
	gitInitRepo(t, neverDir)

	reposTestEnv(t, []config.RepoEntry{
		{Path: freshDir, Name: "alpha"},
		{Path: neverDir, Name: "beta"},
	})
	seedIndexState(t, "alpha", freshHead, false, time.Now().Truncate(time.Second))

	reposJSON = false
	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))

	out := buf.String()
	assert.Contains(t, out, "alpha")
	assert.Contains(t, out, "beta")
	assert.Contains(t, out, "fresh")
	assert.Contains(t, out, "not indexed")
	// The short-SHA prefix of the fresh repo's HEAD must be in the table.
	assert.Contains(t, out, freshHead[:12])
	// The never-indexed repo shows the placeholder timestamp.
	assert.Contains(t, out, "(never)")
}

// TestRunRepos_NoTrackedRepos exercises the empty-config path for both
// output modes.
func TestRunRepos_NoTrackedRepos(t *testing.T) {
	reposTestEnv(t, nil)

	reposJSON = false
	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	assert.Contains(t, buf.String(), "(no tracked repos)")

	reposJSON = true
	t.Cleanup(func() { reposJSON = false })
	cmd, buf = newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	var got []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Empty(t, got)
}

// TestRunRepos_IndexStateFreshness covers the freshness source: the
// daemon's repo_index_state rows, keyed by repo prefix.
func TestRunRepos_IndexStateFreshness(t *testing.T) {
	base := t.TempDir()
	freshDir := filepath.Join(base, "alpha")
	staleDir := filepath.Join(base, "beta")
	dirtyDir := filepath.Join(base, "gamma")
	neverDir := filepath.Join(base, "delta")

	freshHead := gitInitRepo(t, freshDir)
	oldHead := gitInitRepo(t, staleDir)
	dirtyHead := gitInitRepo(t, dirtyDir)
	gitInitRepo(t, neverDir)

	reposTestEnv(t, []config.RepoEntry{
		{Path: freshDir, Name: "alpha"},
		{Path: staleDir, Name: "beta"},
		{Path: dirtyDir, Name: "gamma"},
		{Path: neverDir, Name: "delta"},
	})

	indexedAt := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	// alpha: indexed at the exact current HEAD → fresh.
	seedIndexState(t, "alpha", freshHead, false, indexedAt)
	// beta: indexed at the old HEAD, then HEAD advances → stale.
	seedIndexState(t, "beta", oldHead, false, indexedAt)
	newHead := gitCommitMore(t, staleDir, "second.txt")
	require.NotEqual(t, oldHead, newHead)
	// gamma: indexed at the current HEAD but from a dirty tree → fresh + dirty.
	seedIndexState(t, "gamma", dirtyHead, true, indexedAt)
	// delta: no row → never indexed.

	reposJSON = true
	t.Cleanup(func() { reposJSON = false })

	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))

	var got []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 4)
	byName := map[string]repoStatus{}
	for _, e := range got {
		byName[e.Name] = e
	}

	alpha := byName["alpha"]
	assert.True(t, alpha.Indexed)
	assert.False(t, alpha.Stale, "index commit matches HEAD → fresh")
	assert.Equal(t, freshHead, alpha.IndexedCommit)
	assert.False(t, alpha.IndexedDirty)
	require.NotNil(t, alpha.LastIndexed)
	assert.Equal(t, indexedAt.Unix(), alpha.LastIndexed.Unix())

	beta := byName["beta"]
	assert.True(t, beta.Indexed)
	assert.True(t, beta.Stale, "HEAD advanced past the indexed commit → stale")
	assert.Equal(t, oldHead, beta.IndexedCommit)
	assert.Equal(t, newHead, beta.HeadCommit)

	gamma := byName["gamma"]
	assert.True(t, gamma.Indexed)
	assert.False(t, gamma.Stale, "a dirty-tree index whose commit matches HEAD is still fresh")
	assert.True(t, gamma.IndexedDirty, "dirty provenance is surfaced")

	delta := byName["delta"]
	assert.False(t, delta.Indexed, "no repo_index_state row → never indexed")
	assert.True(t, delta.Stale)
	assert.Nil(t, delta.LastIndexed)
}

// TestRunRepos_IndexStateLoneRepoEmptyPrefix covers single-repo (lone) mode,
// where the index is keyed under the empty repo prefix. A single tracked
// repo must match that "" row even though its resolved prefix is its name.
func TestRunRepos_IndexStateLoneRepoEmptyPrefix(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "solo")
	head := gitInitRepo(t, dir)

	reposTestEnv(t, []config.RepoEntry{{Path: dir, Name: "solo"}})
	indexedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	// Lone-repo mode writes the freshness row under the empty prefix.
	seedIndexState(t, "", head, false, indexedAt)

	reposJSON = true
	t.Cleanup(func() { reposJSON = false })
	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))

	var got []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 1)
	assert.True(t, got[0].Indexed, "the empty-prefix lone-repo row must count for a single tracked repo")
	assert.False(t, got[0].Stale)
	assert.Equal(t, head, got[0].IndexedCommit)
}

// TestRunRepos_UnseededRepoIsNeverIndexed proves a repo with no
// repo_index_state row reports as never indexed and stale, even when a
// sibling repo in the same config does have one.
func TestRunRepos_UnseededRepoIsNeverIndexed(t *testing.T) {
	base := t.TempDir()
	dirA := filepath.Join(base, "one")
	dirB := filepath.Join(base, "two")
	headA := gitInitRepo(t, dirA)
	gitInitRepo(t, dirB)

	reposTestEnv(t, []config.RepoEntry{
		{Path: dirA, Name: "one"},
		{Path: dirB, Name: "two"},
	})
	seedIndexState(t, "one", headA, false, time.Now().Add(-time.Minute).Truncate(time.Second))

	reposJSON = true
	t.Cleanup(func() { reposJSON = false })
	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))

	var got []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	byName := map[string]repoStatus{}
	for _, e := range got {
		byName[e.Name] = e
	}
	one := byName["one"]
	assert.True(t, one.Indexed)
	assert.Equal(t, headA, one.IndexedCommit)
	assert.False(t, one.Stale)

	two := byName["two"]
	assert.False(t, two.Indexed, "no freshness row → never indexed")
	assert.True(t, two.Stale)
	assert.Nil(t, two.LastIndexed)
}

// TestRunRepos_CorruptIndexStoreIsNotAnUnindexedOne separates the two states
// the command used to conflate. Every failure to read the freshness store
// degraded to an empty map, so a corrupt store produced a successful run
// reporting each repo as never indexed — a confident claim about the repos,
// made without having looked, that sends the user to re-do work already done.
// Only a store that genuinely is not there may report "never indexed".
func TestRunRepos_CorruptIndexStoreIsNotAnUnindexedOne(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "solo")
	head := gitInitRepo(t, dir)

	reposTestEnv(t, []config.RepoEntry{{Path: dir, Name: "solo"}})
	reposJSON = true
	t.Cleanup(func() { reposJSON = false })

	// No store file at all: nothing has been indexed, and saying so is right.
	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	var got []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 1)
	assert.False(t, got[0].Indexed, "an absent store means the repo really has never been indexed")

	// A store that is present but is not a database: the command has no idea
	// whether the repo is indexed, and must say so instead of guessing.
	require.NoError(t, os.MkdirAll(filepath.Dir(reposBackendPath), 0o755))
	require.NoError(t, os.WriteFile(reposBackendPath, []byte("this is not a sqlite database"), 0o600))

	cmd, buf = newReposCmd()
	err := runRepos(cmd, nil)
	require.Error(t, err, "a corrupt freshness store must fail the command, not report every repo unindexed")
	assert.Contains(t, err.Error(), reposBackendPath, "the error should name the store it could not read")
	assert.Empty(t, buf.String(), "a failed read must not also print a repo listing")

	// The seeded, readable store still works — the guard rejects unreadable
	// stores, not every store.
	require.NoError(t, os.Remove(reposBackendPath))
	seedIndexState(t, "solo", head, false, time.Now().Truncate(time.Second))
	cmd, buf = newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 1)
	assert.True(t, got[0].Indexed)
}

// TestRunRepos_FollowsTheDaemonRecordedBackendPath exercises the production
// routing. A daemon started with --backend-path writes its freshness rows to a
// store the platform default does not name, and `gortex repos` had no way to
// learn that — it always read the default and reported every repo the daemon
// had indexed as never indexed. The command must follow the store the running
// daemon recorded, with an explicit flag still winning over it.
func TestRunRepos_FollowsTheDaemonRecordedBackendPath(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "solo")
	head := gitInitRepo(t, dir)

	reposTestEnv(t, []config.RepoEntry{{Path: dir, Name: "solo"}})
	// Isolate the platform default so the fallback can never reach the
	// developer's real store, and so "not found there" is a real answer.
	t.Setenv("XDG_DATA_HOME", filepath.Join(base, "data"))
	t.Setenv("GORTEX_DAEMON_STATEFILE", filepath.Join(base, "daemon.state.json"))

	// The daemon's store: somewhere only its own record names.
	daemonStore := filepath.Join(base, "daemon-store", "custom.sqlite")
	indexedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	seedIndexStateAt(t, daemonStore, "solo", head, false, indexedAt)

	// Production shape: no --backend-path on this command at all.
	prevFlag := reposBackendPath
	reposBackendPath = ""
	t.Cleanup(func() { reposBackendPath = prevFlag })

	reposJSON = true
	t.Cleanup(func() { reposJSON = false })

	// With no daemon record, the default store has nothing — the repo really
	// does look unindexed from here.
	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	var got []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 1)
	require.False(t, got[0].Indexed, "without a daemon record only the default store is in scope")

	// A live daemon recording its resolved store is what routes the command.
	require.NoError(t, daemon.WriteRuntimeState(daemon.RuntimeState{BackendPath: daemonStore}))
	t.Cleanup(daemon.RemoveRuntimeState)

	cmd, buf = newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 1)
	assert.True(t, got[0].Indexed, "repos must read the store the running daemon recorded")
	assert.False(t, got[0].Stale)
	assert.Equal(t, head, got[0].IndexedCommit)
	require.NotNil(t, got[0].LastIndexed)
	assert.Equal(t, indexedAt.Unix(), got[0].LastIndexed.Unix())

	// An explicit flag still outranks the daemon's record.
	reposBackendPath = filepath.Join(base, "explicit", "explicit.sqlite")
	cmd, buf = newReposCmd()
	require.NoError(t, runRepos(cmd, nil))
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 1)
	assert.False(t, got[0].Indexed, "--backend-path must win over the daemon's recorded store")
}

// TestReposBackendPathFlagIsRegistered pins the flag on the command: the
// resolution order documents it as the explicit override, and a variable with
// no flag behind it is only reachable from tests.
func TestReposBackendPathFlagIsRegistered(t *testing.T) {
	flag := reposCmd.Flags().Lookup("backend-path")
	require.NotNil(t, flag, "`gortex repos` must accept --backend-path")
	assert.NotEmpty(t, flag.Usage, "the flag needs help text — it is how users learn the resolution order")
}

// TestShortSHA covers the table SHA abbreviation helper.
func TestShortSHA(t *testing.T) {
	assert.Equal(t, "(none)", shortSHA(""))
	assert.Equal(t, "abc", shortSHA("abc"))
	assert.Equal(t, "0123456789ab", shortSHA("0123456789abcdef0123"))
}

// TestRunWorkspaceList_JSON covers the --json flag added to
// `gortex workspace list`: the JSON array carries each repo's resolved
// workspace, project, and source.
func TestRunWorkspaceList_JSON(t *testing.T) {
	root := t.TempDir()
	repoGlobal := filepath.Join(root, "with-global")
	repoYAML := filepath.Join(root, "with-yaml")
	repoDefault := filepath.Join(root, "plain")
	for _, d := range []string{repoGlobal, repoYAML, repoDefault} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	// with-yaml declares its workspace in .gortex.yaml.
	require.NoError(t, os.WriteFile(
		filepath.Join(repoYAML, ".gortex.yaml"),
		[]byte("workspace: yaml-ws\nproject: yaml-proj\n"), 0o644))

	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: repoGlobal, Name: "with-global", Workspace: "global-ws", Project: "global-proj"},
		{Path: repoYAML, Name: "with-yaml"},
		{Path: repoDefault, Name: "plain"},
	}}
	gcPath := filepath.Join(root, "config.yaml")
	gc.SetConfigPath(gcPath)
	require.NoError(t, gc.Save())

	prevCfg := cfgFile
	cfgFile = gcPath
	t.Cleanup(func() { cfgFile = prevCfg })

	workspaceListJSON = true
	t.Cleanup(func() { workspaceListJSON = false })

	cmd, buf := newReposCmd()
	require.NoError(t, runWorkspaceList(cmd, nil))

	var got []workspaceListEntry
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got, 3)

	byRepo := map[string]workspaceListEntry{}
	for _, e := range got {
		byRepo[e.Repo] = e
	}

	g := byRepo["with-global"]
	assert.Equal(t, "global-ws", g.Workspace)
	assert.Equal(t, "global-proj", g.Project)
	assert.Equal(t, "global", g.Source)

	y := byRepo["with-yaml"]
	assert.Equal(t, "yaml-ws", y.Workspace)
	assert.Equal(t, "yaml-proj", y.Project)
	assert.Equal(t, ".gortex.yaml", y.Source)

	p := byRepo["plain"]
	assert.Equal(t, "default", p.Source)
	assert.Contains(t, p.Workspace, "default")
}

// TestRunRepos_DeclaresItsFreshnessIsBaseOnly pins the declaration, not the
// limitation.
//
// `gortex repos` reads repo_index_state through a door that is base-only by
// construction (store_sqlite.RepoIndexStateBaseViewGen), so a daemon serving
// routed worktree views reports those working copies from their family's BASE
// rows. That is deliberate and stays. What must not happen is presenting it as
// the checkout's own freshness, so every output shape says what it is:
// --json carries freshness_scope on every entry, the table form declares it,
// and the help text explains it.
func TestRunRepos_DeclaresItsFreshnessIsBaseOnly(t *testing.T) {
	base := t.TempDir()
	repoDir := filepath.Join(base, "alpha")
	head := gitInitRepo(t, repoDir)
	reposTestEnv(t, []config.RepoEntry{{Path: repoDir, Name: "alpha"}})
	seedIndexState(t, "alpha", head, false, time.Now().Truncate(time.Second))

	t.Run("json entries carry the scope", func(t *testing.T) {
		reposJSON = true
		t.Cleanup(func() { reposJSON = false })
		cmd, buf := newReposCmd()
		require.NoError(t, runRepos(cmd, nil))

		var got []repoStatus
		require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
		require.Len(t, got, 1)
		assert.Equal(t, daemon.ProbeViewBase, got[0].FreshnessScope,
			"the scope must be named in the machine-readable shape, with the probe surface's own word for this corpus")

		// Emitted unconditionally: an absent field means "an older gortex that
		// did not say", which a consumer must be able to tell from "base".
		var raw []map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &raw))
		_, present := raw[0]["freshness_scope"]
		assert.True(t, present, "freshness_scope must not be omitted when it holds the default")
	})

	t.Run("the table form declares it", func(t *testing.T) {
		reposJSON = false
		cmd, buf := newReposCmd()
		require.NoError(t, runRepos(cmd, nil))
		out := buf.String()
		assert.Contains(t, out, "base",
			"the table form must name the corpus its freshness columns describe: %q", out)
		assert.Contains(t, out, "explain-view",
			"the note must point at the verb that answers the worktree's own view: %q", out)
	})

	t.Run("the help text explains it", func(t *testing.T) {
		assert.Contains(t, reposCmd.Long, "BASE corpus only",
			"the command's own help must declare the scope of what it reports")
		assert.Contains(t, reposCmd.Long, "explain-view")
	})
}

// TestReposFreshnessScopeIsDerivedFromTheReadDoor keeps the declaration and the
// predicate from drifting apart: the label is computed from the same constant
// the SQL predicate is built with, so a door that started reading a different
// generation would relabel itself rather than keep saying "base".
func TestReposFreshnessScopeIsDerivedFromTheReadDoor(t *testing.T) {
	assert.Equal(t, daemon.ProbeViewBase, reposScopeName(store_sqlite.RepoIndexStateBaseViewGen))
	assert.Equal(t, "view_gen:7", reposScopeName(7),
		"a non-base generation must not keep the base label")
}
