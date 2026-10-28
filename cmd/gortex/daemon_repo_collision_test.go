package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
)

func TestConfiguredCollisionProbeAndDispatcherPrecedence(t *testing.T) {
	owner, loser := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	for _, root := range []string{owner, loser} {
		require.NoError(t, os.MkdirAll(root, 0o755))
	}
	d, mi := trackedPathMCPSetup(t, owner)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	entries := []config.RepoEntry{{Path: owner}, {Path: loser}}
	mi.ExpectRepos(entries)
	cm, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	cm.Global().Repos = entries // reproduce a legacy conflicting configuration
	c := &realController{multiIndexer: mi, configManager: cm}
	probe, err := c.Probe(t.Context())
	require.NoError(t, err)
	var found bool
	for _, rec := range probe.TrackedRepos {
		if rec.Path == loser {
			found = true
			require.Equal(t, daemon.RepoStatePrefixCollision, rec.State)
			require.Equal(t, daemon.ErrPrefixCollision, rec.ErrorCode)
			require.Equal(t, owner, rec.OwnerPath)
		}
	}
	require.True(t, found)
	sess := &daemon.Session{ID: "prefix-collision", CWD: loser}
	d.SessionStarted(sess, func([]byte) error { return nil })
	t.Cleanup(func() { d.SessionEnded(sess) })
	init := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"collision","version":"1"}}}`)
	body, err := json.Marshal(init)
	require.NoError(t, err)
	require.Contains(t, string(body), "prefix_collision")
	require.NotContains(t, string(body), "already configured")
	reply := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_symbols","arguments":{"query":"OwnerOnly"}}}`)
	data := errorData(t, reply)
	require.Equal(t, daemon.ErrPrefixCollision, data["error_code"])
	require.Equal(t, owner, data["owner_path"])
	require.Equal(t, "present", data["owner_state"])
	require.NotContains(t, data["suggestion"], "untrack")
	file, err := c.FileCoverage(t.Context(), daemon.FileCoverageParams{Path: filepath.Join(loser, "file.go")})
	require.NoError(t, err)
	require.Equal(t, daemon.RepoStatePrefixCollision, file.RepoState)
	require.False(t, file.Tracked)
	require.False(t, file.Answered)
}
