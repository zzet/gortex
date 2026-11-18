package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// newLoadingServer wires a server whose daemon has announced repo as
// configured but not registered it yet.
func newLoadingServer(t *testing.T) (*Server, *indexer.MultiIndexer, string) {
	t.Helper()
	repo := wsRepo(t, "repo-l", "lambda", "LambdaThing")
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(g, testRegistry(), bm, cm, zap.NewNop())
	mi.ExpectRepos([]config.RepoEntry{{Path: repo, Name: "repo-l"}})
	eng := query.NewEngine(g)
	eng.SetSearch(bm)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm})
	return srv, mi, repo
}

// A session that resolves its scope while its repository is loading must not
// keep the blind sentinel once the repository registers.
func TestSessionScope_DoesNotCacheALoadingRepository(t *testing.T) {
	srv, mi, repo := newLoadingServer(t)
	ctx := sessionCtx("s-loading", repo)

	ws, _, bound := srv.sessionScope(ctx)
	require.True(t, bound)
	assert.True(t, strings.HasPrefix(ws, unresolvedWorkspacePrefix), "a loading repository has no scope yet: %q", ws)

	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repo, Name: "repo-l"})
	require.NoError(t, err)

	ws, _, bound = srv.sessionScope(ctx)
	require.True(t, bound)
	assert.Equal(t, "lambda", ws, "the next resolution must bind to the registered repository without a reconnect")
}

func TestStateAwareInstructions_LoadingAndUnavailable(t *testing.T) {
	srv, mi, repo := newLoadingServer(t)

	instr := srv.stateAwareInstructions(repo)
	assert.Contains(t, instr, "LOADING repo-l")
	assert.Contains(t, instr, "no reconnect is needed")
	assert.Contains(t, instr, serverInstructions, "the ordinary instructions follow the loading preamble")
	assert.NotContains(t, instr, "INACTIVE")
	assert.NotContains(t, instr, "gortex track")

	mi.StartRepoLoad(repo)
	mi.FinishRepoLoad(repo, assert.AnError)
	instr = srv.stateAwareInstructions(repo)
	assert.Contains(t, instr, "could not load")
	assert.Contains(t, instr, assert.AnError.Error())
}

func TestLegacyToolForCall_ResolvesFacadeOperations(t *testing.T) {
	srv, _, _ := newLoadingServer(t)
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"workspace", map[string]any{"operation": "index"}, "index_health"},
		{"workspace", map[string]any{"operation": "graph"}, "graph_stats"},
		{"workspace_admin", map[string]any{"operation": "track"}, "track_repository"},
		{"capabilities", nil, "capabilities"},
		{"search_symbols", map[string]any{"query": "x"}, "search_symbols"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, srv.LegacyToolForCall(tc.name, tc.args), "%s %v", tc.name, tc.args)
	}
}
