package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
)

func TestReconcileUnloadedRepos_Warmup(t *testing.T) {
	path := t.TempDir()
	entry := config.RepoEntry{Path: path, Name: "repo"}
	for _, tt := range []struct {
		name     string
		ready    bool
		missing  bool
		pathless bool
		loaded   []daemon.TrackedRepoStatus
		want     []daemon.TrackedRepoStatus
	}{
		{
			name: "warming with existing path",
			want: []daemon.TrackedRepoStatus{{Prefix: "repo", Path: path, Name: "repo",
				Workspace: "repo", WorkspaceProject: "repo", Unloaded: true, Loading: true}},
		},
		{
			name: "warming with missing path", missing: true,
			want: []daemon.TrackedRepoStatus{{Prefix: "repo", Path: path, Name: "repo",
				Workspace: "repo", WorkspaceProject: "repo", Unloaded: true, Missing: true}},
		},
		{
			name: "ready with existing path", ready: true,
			want: []daemon.TrackedRepoStatus{{Prefix: "repo", Path: path, Name: "repo",
				Workspace: "repo", WorkspaceProject: "repo", Unloaded: true}},
		},
		{
			name:   "already loaded by path",
			loaded: []daemon.TrackedRepoStatus{{Prefix: "worktree-instance", Path: path}},
		},
		{
			name:   "another root with the same prefix remains visible",
			loaded: []daemon.TrackedRepoStatus{{Prefix: "repo", Path: filepath.Join(path, "worktree")}},
			want: []daemon.TrackedRepoStatus{{Prefix: "repo", Path: path, Name: "repo",
				Workspace: "repo", WorkspaceProject: "repo", Unloaded: true, Loading: true}},
		},
		{
			name:     "legacy pathless configuration can match by prefix",
			pathless: true,
			loaded:   []daemon.TrackedRepoStatus{{Prefix: "repo"}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			configured := entry
			if tt.pathless {
				configured.Path = ""
			}
			got := reconcileUnloadedRepos([]config.RepoEntry{configured}, map[string]bool{path: tt.missing}, tt.loaded, tt.ready)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReconcileUnloadedReposKeepsACollidingRootVisible(t *testing.T) {
	owner, loser := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	rows := reconcileUnloadedRepos([]config.RepoEntry{{Path: loser}}, map[string]bool{loser: false},
		[]daemon.TrackedRepoStatus{{Path: owner, Prefix: "src"}}, true)
	require.Len(t, rows, 1)
	require.Equal(t, loser, rows[0].Path)
	require.True(t, rows[0].Unloaded)
}

func TestControllerStatus_LoadingFollowsReadiness(t *testing.T) {
	path := t.TempDir()
	c := probeController(t, "repos:\n  - path: "+path+"\n    name: repo\n")
	warming, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, warming.TrackedRepos, 1)
	assert.False(t, warming.Ready)
	assert.True(t, warming.TrackedRepos[0].Loading)
	assert.True(t, warming.TrackedRepos[0].Unloaded)
	assert.False(t, warming.TrackedRepos[0].Missing)
	assert.Empty(t, warming.Workspaces, "synthetic loading rows do not enter workspace totals")

	c.MarkReady(time.Second)
	ready, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, ready.TrackedRepos, 1)
	assert.True(t, ready.Ready)
	assert.False(t, ready.TrackedRepos[0].Loading)
	assert.True(t, ready.TrackedRepos[0].Unloaded)
}

func TestRepoLoadingLabels(t *testing.T) {
	for _, tt := range []struct {
		name   string
		row    daemon.TrackedRepoStatus
		suffix string
		label  string
		item   string
	}{
		{"missing wins", daemon.TrackedRepoStatus{Missing: true, Loading: true, Unloaded: true},
			"MISSING — path no longer exists on disk", "MISSING", "MISSING — path deleted"},
		{"loading wins", daemon.TrackedRepoStatus{Loading: true, Unloaded: true, FailedFiles: 2, LastIndex: 1},
			"(loading — warmup in progress)", "loading", "loading (warmup)"},
		{"loading without unloaded", daemon.TrackedRepoStatus{Loading: true},
			"(loading — warmup in progress)", "loading", "loading (warmup)"},
		{"unloaded", daemon.TrackedRepoStatus{Unloaded: true},
			"(not indexed — tracked in config, no index held)", "not indexed", "not indexed"},
		{"healthy", daemon.TrackedRepoStatus{Files: 3, Nodes: 4, Edges: 5},
			"(3 files, 4 nodes, 5 edges)", "ok", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.suffix, repoStatusSuffix(tt.row))
			assert.Equal(t, tt.label, repoStateLabel(tt.row))
			assert.Equal(t, tt.item, repoItemState(tt.row))
		})
	}
}

func TestLoadingRepoRetainsIndexHealth(t *testing.T) {
	st := daemon.StatusResponse{TrackedRepos: []daemon.TrackedRepoStatus{
		{Prefix: "repo", Unloaded: true, Loading: true},
	}}
	called := false
	applyIndexFailureStatus(&st, func(prefix string) (int, int, error) {
		called = true
		assert.Equal(t, "repo", prefix)
		return 2, 1, nil
	})
	assert.True(t, called, "loading rows still read their persisted failure ledger")
	assert.True(t, st.IndexDegraded)
	assert.Empty(t, st.IndexHealthError)
	assert.Equal(t, 2, st.FailedFiles)
	assert.Equal(t, 1, st.UnreadableFiles)
	assert.True(t, st.TrackedRepos[0].IndexDegraded)
	assert.Equal(t, "loading", repoStateLabel(st.TrackedRepos[0]))
	var out bytes.Buffer
	renderMissingRepoWarning(&out, st.TrackedRepos)
	renderEmptyIndexWarning(&out, st.TrackedRepos)
	assert.Empty(t, out.String(), "loading alone is neither missing nor an empty index")
	renderIndexFailureWarning(&out, st.TrackedRepos)
	assert.Contains(t, out.String(), "repo: index degraded — 2 failed files (1 unreadable)")
	out.Reset()
	renderDaemonHeader(&out, st)
	assert.Contains(t, out.String(), "warming up")
	assert.Contains(t, out.String(), "degraded — 2 failed files (1 unreadable)")
	out.Reset()
	renderDaemonRepos(&out, st)
	assert.Contains(t, out.String(), "loading")
	assert.NotContains(t, out.String(), "not indexed")
	assert.Contains(t, out.String(), "repo: index degraded — 2 failed files (1 unreadable)")
}
