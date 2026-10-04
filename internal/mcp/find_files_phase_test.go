package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

func TestFindFilesAutoPrimaryUsesSelectedProjectionAndDebugPhases(t *testing.T) {
	v := newViewStack(t)
	tracked := &findFilesProjectionStore{Store: v.store}
	v.srv.graph = tracked
	installContractCoreKindTestRuntime(t, v.srv)
	logs, observed := observer.New(zap.DebugLevel)
	v.srv.logger = zap.New(logs)
	ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), v.repoRoot)
	selected, err := v.srv.selectRequestView(ctx, graphview.Selector{Kind: graphview.SelectorAuto}, requestViewPolicy{})
	require.NoError(t, err)
	require.Nil(t, selected, "primary Auto reads the shared indexed corpus")
	ctx, overlay, err := v.srv.prepareOverlayRequest(ctx)
	require.NoError(t, err)
	require.Nil(t, overlay)
	require.Implements(t, (*graph.ScopedNodeProjectionSequencer)(nil), v.srv.readerFor(ctx))
	res, err := v.srv.handleFindFiles(ctx, makeReq("find_files", map[string]any{"query": "edit.go", "repo": "repo", "path": "edit.go"}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Zero(t, tracked.globalReads)
	require.Equal(t, []string{"", "repo"}, tracked.projectedRepos)
	phases := observed.FilterMessage("find_files phases").All()
	require.Len(t, phases, 1)
	fields := phases[0].ContextMap()
	require.Equal(t, true, fields["node_projection_used"])
	require.Equal(t, true, fields["node_projection_supported"])
	require.Greater(t, fields["visited"].(int64), int64(0))
	v.srv.logger = zap.NewNop()
	before := tracked.globalReads
	again, err := v.srv.handleFindFiles(ctx, makeReq("find_files", map[string]any{"query": "edit.go", "repo": "repo", "path": "edit.go"}))
	require.NoError(t, err)
	require.Equal(t, res, again, "debug phases must not change output")
	require.Equal(t, before, tracked.globalReads)
}
