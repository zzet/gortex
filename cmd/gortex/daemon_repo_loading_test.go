package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
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
		return nil, &repoLoadingError{repo: "app", details: repoLoadingDetails{Phase: "parallel_parse", Percent: new(15)}}
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (f *fakeLoadingExecutor) Close() error { return nil }

func TestCallToolWaitingForLoad(t *testing.T) {
	t.Run("no wait fails fast with the loading error", func(t *testing.T) {
		exec := &fakeLoadingExecutor{loadingCalls: 1}
		_, err := callToolWaitingForLoad(exec, "search_symbols", nil, 0, os.Stderr)
		var loading *repoLoadingError
		require.ErrorAs(t, err, &loading)
		assert.Equal(t, 1, exec.calls)
	})
	t.Run("a wait retries until the repository is ready", func(t *testing.T) {
		exec := &fakeLoadingExecutor{loadingCalls: 2}
		out, err := callToolWaitingForLoad(exec, "search_symbols", nil, 10*time.Second, os.Stderr)
		require.NoError(t, err)
		assert.JSONEq(t, `{"ok":true}`, string(out))
		assert.Equal(t, 3, exec.calls)
	})
	t.Run("running out of wait reports still loading", func(t *testing.T) {
		exec := &fakeLoadingExecutor{loadingCalls: 1 << 30}
		_, err := callToolWaitingForLoad(exec, "search_symbols", nil, 1500*time.Millisecond, os.Stderr)
		var loading *repoLoadingError
		require.ErrorAs(t, err, &loading)
		assert.Positive(t, loading.waited)
		assert.Contains(t, err.Error(), "still loading after waiting")
	})
}

func stalledLoadingRetry(t *testing.T, tool string, wait time.Duration) (<-chan error, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "gxw")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	retrySeen := make(chan struct{})
	releasePeer := make(chan struct{})
	release := sync.OnceFunc(func() { close(releasePeer) })
	t.Cleanup(release)
	peerDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			peerDone <- err
			return
		}
		defer conn.Close()
		peerDone <- func() error {
			reader := bufio.NewReader(conn)
			if _, err := reader.ReadBytes('\n'); err != nil {
				return err
			}
			if err := daemon.WriteJSONLine(conn, daemon.HandshakeAck{OK: true}); err != nil {
				return err
			}
			if _, err := reader.ReadBytes('\n'); err != nil {
				return err
			}
			const refusal = `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"loading","data":{"error_code":"repo_loading","retryable":true,"phase":"migrating","percent":15}}}` + "\n"
			if _, err := io.WriteString(conn, refusal); err != nil {
				return err
			}
			if _, err := reader.ReadBytes('\n'); err != nil {
				return err
			}
			close(retrySeen)
			<-releasePeer
			return nil
		}()
	}()

	client, err := daemon.DialTo(socket, daemon.Handshake{Mode: daemon.ModeMCP})
	require.NoError(t, err)
	exec := &daemonExecutor{client: client}
	t.Cleanup(func() { _ = exec.Close() })
	result := make(chan error, 1)
	go func() {
		_, err := callToolWaitingForLoad(exec, tool, nil, wait, io.Discard)
		result <- err
	}()

	select {
	case <-retrySeen:
	case err := <-peerDone:
		t.Fatalf("socket fixture failed before the retry: %v", err)
	case <-time.After(wait + 2*time.Second):
		t.Fatal("daemon did not receive a real retry after the first typed refusal")
	}
	return result, release
}

func TestLoadingWaitBoundsStalledSocketRetry(t *testing.T) {
	const wait = 2 * loadingWaitPoll
	result, _ := stalledLoadingRetry(t, "search_symbols", wait)
	select {
	case err := <-result:
		var loading *repoLoadingError
		require.ErrorAs(t, err, &loading)
		require.Equal(t, exitTempFail, loading.ExitCode())
		require.Equal(t, "repo_loading", loading.details.ErrorCode)
		require.NotNil(t, loading.details.Retryable)
		require.True(t, *loading.details.Retryable)
		require.Equal(t, "migrating", loading.details.Phase)
		require.NotNil(t, loading.details.Percent)
		require.Equal(t, 15, *loading.details.Percent)
	case <-time.After(wait + time.Second):
		t.Fatal("CLI remained blocked on a Unix socket retry beyond its loading wait")
	}
}

func TestLoadingWaitPreservesUncertainGenericRetry(t *testing.T) {
	const wait = 2 * loadingWaitPoll
	for _, tool := range []string{"write_file", "unrecognized_tool"} {
		t.Run(tool, func(t *testing.T) {
			result, release := stalledLoadingRetry(t, tool, wait)
			select {
			case err := <-result:
				t.Fatalf("retry ended before the peer resolved its outcome: %v", err)
			case <-time.After(wait):
			}
			release()
			select {
			case err := <-result:
				require.ErrorIs(t, err, io.EOF)
			case <-time.After(time.Second):
				t.Fatal("CLI did not report the real peer close")
			}
		})
	}
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
