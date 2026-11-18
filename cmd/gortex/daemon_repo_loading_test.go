package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestProbe_ReportsPerRepoLoadState(t *testing.T) {
	base := t.TempDir()
	ready := filepath.Join(base, "ready")
	loading := filepath.Join(base, "loading")
	failed := filepath.Join(base, "failed")
	missing := filepath.Join(base, "missing")
	projectOnly := filepath.Join(base, "project-only")
	for _, dir := range []string{ready, loading, failed, projectOnly} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	require.NoError(t, writeFile(filepath.Join(ready, "r.go"), "package r\n"))
	c := probeController(t, "repos:\n"+
		"  - path: "+ready+"\n"+
		"  - path: "+loading+"\n"+
		"  - path: "+failed+"\n"+
		"  - path: "+missing+"\n"+
		"projects:\n  side:\n    repos:\n      - path: "+projectOnly+"\n")
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	mi := indexer.NewMultiIndexer(graph.New(), reg, search.NewNull(), c.configManager, zap.NewNop())
	c.multiIndexer = mi
	mi.ExpectRepos(c.configManager.Global().Repos)
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: ready})
	require.NoError(t, err)
	mi.StartRepoLoad(failed)
	mi.FinishRepoLoad(failed, errors.New("indexing exploded"))

	resp, err := c.Probe(context.Background())
	require.NoError(t, err)
	states := map[string]daemon.ProbeRepo{}
	for _, repo := range resp.TrackedRepos {
		states[repo.Path] = repo
	}
	assert.Equal(t, daemon.RepoStateReady, states[ready].State)
	assert.Equal(t, daemon.RepoStateLoading, states[loading].State)
	assert.Equal(t, daemon.RepoStateFailed, states[failed].State)
	assert.Equal(t, "indexing exploded", states[failed].StateReason)
	assert.Equal(t, daemon.RepoStateMissing, states[missing].State)
	assert.Empty(t, states[projectOnly].State, "warmup never loads a project-only entry, so it must never read as loading")
}

func TestFileCoverage_CarriesLoadingRepoState(t *testing.T) {
	_, mi, registered, loading := loadingDispatcherSetup(t)
	c := &realController{multiIndexer: mi}

	got, err := c.FileCoverage(context.Background(), daemon.FileCoverageParams{Path: filepath.Join(loading, "b.go")})
	require.NoError(t, err)
	assert.Equal(t, daemon.RepoStateLoading, got.RepoState)
	assert.Equal(t, "loading", got.Repo)

	got, err = c.FileCoverage(context.Background(), daemon.FileCoverageParams{Path: filepath.Join(registered, "a.go")})
	require.NoError(t, err)
	assert.Empty(t, got.RepoState, "a registered repository's coverage carries no loading state")

	dir, err := c.DirCoverage(context.Background(), daemon.DirCoverageParams{Path: loading})
	require.NoError(t, err)
	assert.Equal(t, daemon.RepoStateLoading, dir.RepoState)
}

func TestExtractToolResult_RepoLoadingIsTypedAndTempFail(t *testing.T) {
	frame, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"error": map[string]any{"code": -32000, "message": "repository loading", "data": map[string]any{
			"error_code": "repo_loading", "retryable": true, "repo": "app", "phase": "parallel_parse", "percent": 15,
		}},
	})
	_, err := extractToolResult(frame)
	var loading *repoLoadingError
	require.ErrorAs(t, err, &loading)
	assert.Equal(t, exitTempFail, loading.ExitCode())
	assert.Contains(t, err.Error(), "still loading app")
	assert.Contains(t, err.Error(), "parallel_parse, 15%")
	assert.NotContains(t, err.Error(), "gortex track")
	assert.NotErrorIs(t, err, ErrRepoNotTracked, "a loading repository must never fall into the not-tracked remedy")
}

// fakeLoadingExecutor answers repo_loading a fixed number of times, then
// succeeds.
type fakeLoadingExecutor struct {
	loadingCalls int
	calls        int
}

func (f *fakeLoadingExecutor) CallTool(context.Context, string, map[string]any) (json.RawMessage, error) {
	f.calls++
	if f.calls <= f.loadingCalls {
		return nil, &repoLoadingError{repo: "app", phase: "parallel_parse", percent: 15}
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (f *fakeLoadingExecutor) Close() error { return nil }

func TestCallToolWaitingForLoad(t *testing.T) {
	t.Run("no wait fails fast with the loading error", func(t *testing.T) {
		exec := &fakeLoadingExecutor{loadingCalls: 1}
		_, err := callToolWaitingForLoad(exec, "search_symbols", nil, 0)
		var loading *repoLoadingError
		require.ErrorAs(t, err, &loading)
		assert.Equal(t, 1, exec.calls)
	})
	t.Run("a wait retries until the repository is ready", func(t *testing.T) {
		exec := &fakeLoadingExecutor{loadingCalls: 2}
		out, err := callToolWaitingForLoad(exec, "search_symbols", nil, 10*time.Second)
		require.NoError(t, err)
		assert.JSONEq(t, `{"ok":true}`, string(out))
		assert.Equal(t, 3, exec.calls)
	})
	t.Run("running out of wait reports still loading", func(t *testing.T) {
		exec := &fakeLoadingExecutor{loadingCalls: 1 << 30}
		_, err := callToolWaitingForLoad(exec, "search_symbols", nil, 1500*time.Millisecond)
		var loading *repoLoadingError
		require.ErrorAs(t, err, &loading)
		assert.Positive(t, loading.waited)
		assert.Contains(t, err.Error(), "still loading after waiting")
	})
}

// Hooks and scripts run the CLI without a terminal; they must fail fast with
// exit 75 rather than stall for the interactive default.
func TestEffectiveLoadingWait_NonTTYFailsFast(t *testing.T) {
	assert.Zero(t, effectiveLoadingWait(), "go test's stdout is not a terminal")
}

func TestTrackedRepoLoading(t *testing.T) {
	st := daemon.StatusResponse{TrackedRepos: []daemon.TrackedRepoStatus{
		{Path: "/work/app", Loading: true},
		{Path: "/work/lib"},
	}}
	assert.True(t, trackedRepoLoading(st, "/work/app"))
	assert.False(t, trackedRepoLoading(st, "/work/lib"))
	assert.False(t, trackedRepoLoading(st, "/work/other"))
}
