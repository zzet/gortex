//go:build !windows

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

// Original C6 native crash fixture support; no abandoned minimal C5 or private
// per-request source/lease diagnostic is part of this shipping support file.
func freshnessLifecycleRepo(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0700))
	for name, body := range map[string]string{
		"target.go":  "package p\nfunc Target() int { return 1 }\n",
		"caller.go":  "package p\nfunc Caller() int { return Target() }\n",
		"other_a.go": "package p\nfunc UnrelatedA() int { return 0 }\n",
		"other_b.go": "package p\nfunc UnrelatedB() int { return 0 }\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0600))
	}
	checkoutMutationGit(t, root, "init", "-q")
	checkoutMutationGit(t, root, "add", ".")
	checkoutMutationGit(t, root, "commit", "-q", "-m", "freshness baseline")
}

func freshnessLifecycleTrackedServer(t *testing.T, store *store_sqlite.Store, root string, debounce int, extractors ...parser.Extractor) (*Server, *indexer.MultiWatcher) {
	t.Helper()
	entry := config.RepoEntry{Name: "repo", Path: root}
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{entry}}
	cfgPath := filepath.Join(filepath.Dir(root), "config.yaml")
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)
	reg := parser.NewRegistry()
	var extractor parser.Extractor = languages.NewGoExtractor()
	if len(extractors) > 0 {
		require.Len(t, extractors, 1)
		require.NotNil(t, extractors[0])
		extractor = extractors[0]
	}
	reg.Register(extractor)
	mi := indexer.NewMultiIndexer(store, reg, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	leases := graphview.NewLeaseManager()
	mi.SetOutputGenerationAuthority(indexer.NewOutputGenerationAuthority(leases))
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer: mi, ConfigManager: cm, Graph: store, Logger: zap.NewNop(), ViewLeases: leases,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lifecycle.Close()) })
	registered, err := lifecycle.Register(t.Context(), entry, store_sqlite.IntentSourceMCPTrack)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	require.NotEmpty(t, registered.CheckoutID)
	require.NotEmpty(t, registered.Incarnation)
	require.NotEmpty(t, registered.GraphID)
	w, err := indexer.NewConfigManagedMultiWatcher(mi, map[string]config.WatchConfig{"repo": {Enabled: true, DebounceMs: debounce}}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, w.Start())
	t.Cleanup(func() { require.NoError(t, w.Stop()) })
	s := NewServer(query.NewEngine(store), store, mi.GetIndexer("repo"), nil, zap.NewNop(), nil,
		MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm, CheckoutLifecycle: lifecycle})
	s.SetWatcher(w)
	s.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases})
	return s, w
}

func freshnessLifecycleSession(t *testing.T, s *Server) context.Context {
	t.Helper()
	const id = "freshness-lifecycle"
	require.NoError(t, s.ConnectSession(id, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(id) })
	ctx := s.WithClientSession(WithSessionID(t.Context(), id), id)
	s.NoteSessionToolPolicy(id, "all", "defer")
	require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"freshness-lifecycle","version":"1"}}}`)))
	return ctx
}

func freshnessLifecycleCall(t *testing.T, s *Server, ctx context.Context, id int, tool string, args map[string]any) map[string]any {
	t.Helper()
	res, err := externalVersionRegisteredCall(s, ctx, id, tool, args)
	if res != nil {
		raw, marshalErr := json.Marshal(res)
		require.NoError(t, marshalErr)
		if len(raw) > 64<<10 && (t.Name() != "TestFreshnessLifecycleUnrelatedTwoEditsPerSecond" || id != 12) {
			t.Logf("ACTUAL_REGISTERED_RESPONSE tool=%s id=%d bytes=%d diagnostic_capped=true prefix=%s", tool, id, len(raw), raw[:64<<10])
		} else {
			t.Logf("ACTUAL_REGISTERED_RESPONSE tool=%s id=%d body=%s", tool, id, raw)
		}
	}
	if err != nil {
		t.Logf("ACTUAL_REGISTERED_ERROR tool=%s id=%d error=%v", tool, id, err)
	}
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError, resultText(res))
	return resultJSON(t, res)
}

func freshnessLifecycleTier(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	fields, ok := body["freshness"].(map[string]any)
	require.True(t, ok, "missing actual tier evidence: %v", body)
	return fields
}

func freshnessLifecycleHasCaller(t *testing.T, body map[string]any, caller string) {
	t.Helper()
	edges, ok := body["edges"].([]any)
	require.True(t, ok, "missing actual callers: %v", body)
	found := false
	for _, raw := range edges {
		edge, ok := raw.(map[string]any)
		if ok && edge["from"] == "repo/caller.go::"+caller && edge["to"] == "repo/target.go::Target" && edge["kind"] == "calls" {
			found = true
		}
	}
	require.True(t, found, "actual caller %s missing: %v", caller, body)
}
