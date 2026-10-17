package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func TestResolveRepoPrefixOrReconcileLogsAutoTrackOrigin(t *testing.T) {
	root := setupMiniRepo(t, "auto-track")
	cm, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: root, Name: "auto-track"}))
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	mi := indexer.NewMultiIndexer(graph.New(), reg, search.NewNull(), cm, logger)
	srv := &Server{multiIndexer: mi, configManager: cm, logger: logger}

	assert.Equal(t, "auto-track", srv.resolveRepoPrefixOrReconcile(context.Background(), root))
	entries := logs.FilterMessage("indexer: full index started").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	assert.Equal(t, "auto-track", fields["repo"])
	assert.Equal(t, "mcp_auto_track", fields["origin"])
	assert.Equal(t, true, fields["first_index"])
	assert.Equal(t, false, fields["shadow"])
	logs.TakeAll()
	assert.Equal(t, "auto-track", srv.resolveRepoPrefixOrReconcile(context.Background(), root))
	assert.Zero(t, logs.FilterMessage("indexer: full index started").Len(), "an already tracked repo must not start indexing")
}
