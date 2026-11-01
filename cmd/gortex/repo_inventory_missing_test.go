package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// Issue #312: a tracked repo whose directory was deleted was never
// flagged anywhere, and the two inventory views drifted apart — `gortex
// daemon status` dropped the repo (its startup index failed, so it never
// reached the indexer registry) while `gortex repos` kept listing it from
// the config. Neither view said the path was gone.
//
// These tests pin both halves of the fix: one inventory across the two
// views, and a MISSING state wherever a dead path shows up.

// --- `gortex repos` ----------------------------------------------------

func TestRepos_DeletedPathReportsMissing(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	gitInitRepo(t, live)
	gone := filepath.Join(root, "gone")
	gitInitRepo(t, gone)
	require.NoError(t, os.RemoveAll(gone))

	reposTestEnv(t, []config.RepoEntry{{Path: live}, {Path: gone}})

	cmd, buf := newReposCmd()
	prevJSON := reposJSON
	reposJSON = true
	t.Cleanup(func() { reposJSON = prevJSON })
	require.NoError(t, runRepos(cmd, nil))

	var entries []repoStatus
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	require.Len(t, entries, 2)

	byName := map[string]repoStatus{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	assert.False(t, byName["live"].Missing, "a live checkout is not missing")
	assert.True(t, byName["gone"].Missing,
		"a tracked repo whose directory was deleted must be flagged missing")
}

// The table is where the eight-day-old ghost of #312 hid behind
// "HEAD (none) … stale". MISSING replaces that, and the remediation
// command is printed verbatim so the fix is copy-pasteable.
func TestRepos_MissingRendersStateAndUntrackHint(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	gitInitRepo(t, live)
	gone := filepath.Join(root, "gone")
	gitInitRepo(t, gone)
	require.NoError(t, os.RemoveAll(gone))

	reposTestEnv(t, []config.RepoEntry{{Path: live}, {Path: gone}})

	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))

	out := buf.String()
	assert.Contains(t, out, "MISSING")
	assert.Contains(t, out, "1 tracked repo no longer exists on disk")
	assert.Contains(t, out, "gortex untrack "+gone)
	assert.NotContains(t, out, "gortex untrack "+live)
}

// A healthy inventory must stay clean — no warning block, no MISSING
// column noise for anyone whose repos are all present.
func TestRepos_AllLiveEmitsNoMissingHint(t *testing.T) {
	live := filepath.Join(t.TempDir(), "live")
	gitInitRepo(t, live)
	reposTestEnv(t, []config.RepoEntry{{Path: live}})

	cmd, buf := newReposCmd()
	require.NoError(t, runRepos(cmd, nil))

	out := buf.String()
	assert.NotContains(t, out, "MISSING")
	assert.NotContains(t, out, "gortex untrack")
}

// --- daemon Status reconciliation --------------------------------------

// newInventoryController builds a controller over a config that tracks
// `repos`, having indexed only what was on disk at index time.
func newInventoryController(t *testing.T, repos []config.RepoEntry) *realController {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: repos}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	mi.ExpectRepos(repos)
	for _, entry := range repos {
		mi.StartRepoLoad(entry.Path)
		_, trackErr := mi.TrackRepoCtx(context.Background(), entry)
		// Missing entries deliberately remain configured but unavailable;
		// inventory/status assertions below must still report those entries.
		mi.FinishRepoLoad(entry.Path, trackErr)
	}

	return &realController{graph: g, multiIndexer: mi, configManager: cm, logger: zap.NewNop()}
}

// The production symptom: after a restart the daemon holds no index for
// the deleted repo, so it vanished from `daemon status` while `gortex
// repos` still listed it. Status must report the config registry's whole
// inventory, marking what it could not load.
func TestControllerStatus_UnloadedConfigRepoStillListed(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	require.NoError(t, os.MkdirAll(live, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(live, "lib.go"),
		[]byte("package lib\n\nfunc A() {}\n"), 0o644))
	gone := filepath.Join(root, "gone")

	// `gone` is tracked in config but was never on disk to index — the
	// same state a daemon reaches after restarting with a deleted repo.
	c := newInventoryController(t, []config.RepoEntry{{Path: live}, {Path: gone}})

	resp, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, resp.TrackedRepos, 2,
		"status must list every configured repo, including ones it could not index")

	byPrefix := map[string]daemon.TrackedRepoStatus{}
	for _, r := range resp.TrackedRepos {
		byPrefix[r.Prefix] = r
	}
	require.Contains(t, byPrefix, "gone")
	assert.True(t, byPrefix["gone"].Unloaded, "the daemon holds no index for it")
	assert.True(t, byPrefix["gone"].Missing, "and its path is gone from disk")
	assert.Equal(t, gone, byPrefix["gone"].Path)

	require.Contains(t, byPrefix, "live")
	assert.False(t, byPrefix["live"].Unloaded)
	assert.False(t, byPrefix["live"].Missing)
}

// The other window in #312: the repo is deleted while the daemon is
// still up, so the row is real (indexer metadata still names the dead
// root) but the checkout is gone. The row must be flagged, not dropped.
func TestControllerStatus_LoadedRepoWithDeletedPathIsFlagged(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "lib.go"),
		[]byte("package lib\n\nfunc A() {}\n"), 0o644))

	c := newInventoryController(t, []config.RepoEntry{{Path: repo}})

	before, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, before.TrackedRepos, 1)
	require.False(t, before.TrackedRepos[0].Missing)

	require.NoError(t, os.RemoveAll(repo))

	after, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, after.TrackedRepos, 1, "the row stays — it is still tracked")
	assert.True(t, after.TrackedRepos[0].Missing)
	assert.False(t, after.TrackedRepos[0].Unloaded,
		"the daemon does still hold this repo's index")
}

// Synthetic rows carry no counts, so they must not enter the
// per-workspace rollup and invent a workspace with zero files.
func TestControllerStatus_UnloadedRepoStaysOutOfWorkspaceRollup(t *testing.T) {
	live := filepath.Join(t.TempDir(), "live")
	require.NoError(t, os.MkdirAll(live, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(live, "lib.go"),
		[]byte("package lib\n\nfunc A() {}\n"), 0o644))
	gone := filepath.Join(t.TempDir(), "gone")

	c := newInventoryController(t, []config.RepoEntry{{Path: live}, {Path: gone}})
	resp, err := c.Status(context.Background())
	require.NoError(t, err)

	for _, ws := range resp.Workspaces {
		assert.NotContains(t, ws.Repos, "gone",
			"an unindexed repo contributes nothing to summarise")
	}
}

// --- daemon status / status renderers ----------------------------------

func TestRenderDaemonRepos_MissingRepoIsShoutedWithRemediation(t *testing.T) {
	st := sampleStatus()
	st.TrackedRepos = append(st.TrackedRepos, daemon.TrackedRepoStatus{
		Prefix:   "fastpanel-api",
		Path:     "/tmp/code/fastpanel-api",
		Unloaded: true,
		Missing:  true,
	})

	var buf bytes.Buffer
	renderDaemonRepos(&buf, st)
	out := buf.String()

	assert.Contains(t, out, "fastpanel-api")
	assert.Contains(t, out, "MISSING")
	assert.Contains(t, out, "state", "the state column appears once a repo is in trouble")
	assert.Contains(t, out, "gortex untrack /tmp/code/fastpanel-api")
}

// The state column is pure noise on a healthy workspace where every row
// would read "ok", so it must stay hidden there.
func TestRenderDaemonRepos_HealthyWorkspaceHasNoStateColumn(t *testing.T) {
	var buf bytes.Buffer
	renderDaemonRepos(&buf, sampleStatus())
	out := buf.String()

	assert.NotContains(t, out, "MISSING")
	assert.NotContains(t, out, "gortex untrack")
	header := strings.SplitN(out, "\n", 5)
	assert.NotContains(t, strings.Join(header, "\n"), " state ")
}

func TestRenderMissingRepoWarning_PluralisesAndListsEach(t *testing.T) {
	var buf bytes.Buffer
	renderMissingRepoWarning(&buf, []daemon.TrackedRepoStatus{
		{Prefix: "a", Path: "/tmp/a", Missing: true},
		{Prefix: "b", Path: "/tmp/b"},
		{Prefix: "c", Path: "/tmp/c", Missing: true},
	})
	out := buf.String()

	assert.Contains(t, out, "2 tracked repos no longer exist on disk")
	assert.NotContains(t, out, "repos no longer exists")
	assert.Contains(t, out, "gortex untrack /tmp/a")
	assert.Contains(t, out, "gortex untrack /tmp/c")
	assert.NotContains(t, out, "/tmp/b")
}

// The watch TUI renders counts, so a MISSING repo would read as an empty
// repo rather than an absent one without an explicit state cell.
func TestRepoItemState(t *testing.T) {
	assert.Equal(t, "MISSING — path deleted", repoItemState(daemon.TrackedRepoStatus{Missing: true}))
	assert.Equal(t, "MISSING — path deleted",
		repoItemState(daemon.TrackedRepoStatus{Missing: true, Unloaded: true}))
	assert.Equal(t, "not indexed", repoItemState(daemon.TrackedRepoStatus{Unloaded: true}))
	assert.Empty(t, repoItemState(daemon.TrackedRepoStatus{Files: 3}),
		"a healthy repo shows its counts, not a state")
}

func TestRepoStateLabel(t *testing.T) {
	assert.Equal(t, "MISSING", repoStateLabel(daemon.TrackedRepoStatus{Missing: true}))
	assert.Equal(t, "MISSING", repoStateLabel(daemon.TrackedRepoStatus{Missing: true, Unloaded: true}),
		"a deleted path is the actionable fact; it outranks 'not indexed'")
	assert.Equal(t, "not indexed", repoStateLabel(daemon.TrackedRepoStatus{Unloaded: true}))
	assert.Equal(t, "ok", repoStateLabel(daemon.TrackedRepoStatus{}))
}
