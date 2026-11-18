package mcp

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

func TestStateAwareInstructionsDirectConfiguredFamilyPrimary(t *testing.T) {
	for _, state := range []string{"not-announced", "loading", "failed"} {
		t.Run(state, func(t *testing.T) {
			primary := filepath.Join(t.TempDir(), "primary")
			gitInitWorktreeRepo(t, primary)
			gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: primary, Name: "primary"}}}
			gc.SetConfigPath(filepath.Join(t.TempDir(), "config.yaml"))
			require.NoError(t, gc.Save())
			cm, err := config.NewConfigManager(gc.ConfigPath())
			require.NoError(t, err)
			store := graph.New()
			mi := indexer.NewMultiIndexer(store, testRegistry(), search.NewNull(), cm, zap.NewNop())
			if state != "not-announced" {
				mi.ExpectRepos(gc.ReposSnapshot())
			}
			if state == "failed" {
				mi.FinishRepoLoad(primary, errors.New("fixture registration failed"))
			}
			srv := NewServer(query.NewEngine(store), store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm})
			t.Cleanup(func() { _ = srv.lifecycle.Close() })
			text := srv.stateAwareInstructions(primary)
			require.NotContains(t, text, "gortex track "+primary)
			require.NotContains(t, strings.ReplaceAll(text, "no reconnect is needed", ""), "reconnect")
			if state == "loading" {
				require.Contains(t, text, "repo_loading")
				require.NotContains(t, text, "discovery is pending")
			} else {
				require.Contains(t, text, "view_building")
				require.Contains(t, text, "discovery is pending")
			}
			if state == "failed" {
				require.Contains(t, text, "fixture registration failed")
			}
		})
	}
}
