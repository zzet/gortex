package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

func guidanceGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "commit.gpgsign=false", "-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}

func guidanceGitFamily(t *testing.T) (string, string) {
	t.Helper()
	primary := filepath.Join(t.TempDir(), "primary")
	linked := filepath.Join(t.TempDir(), "linked")
	require.NoError(t, os.MkdirAll(primary, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(primary, "fixture.go"), []byte("package fixture\nfunc Value() int { return 1 }\n"), 0o600))
	guidanceGit(t, primary, "init", "-q", "-b", "main")
	guidanceGit(t, primary, "add", ".")
	guidanceGit(t, primary, "commit", "-q", "-m", "fixture")
	guidanceGit(t, primary, "worktree", "add", "-q", "-b", "feature", linked)
	return primary, linked
}

type familyGuidanceFixture struct {
	dispatcher      *mcpDispatcher
	mi              *indexer.MultiIndexer
	config          *config.GlobalConfig
	primary, linked string
}

func newFamilyGuidanceFixture(t *testing.T, registered bool, state store_sqlite.CheckoutState) familyGuidanceFixture {
	t.Helper()
	primary, linked := guidanceGitFamily(t)
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: primary, Name: "primary"}}}
	gc.SetConfigPath(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(gc.ConfigPath())
	require.NoError(t, err)
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	mi := indexer.NewMultiIndexer(store, registry, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	if registered {
		_, err := mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: primary, Name: "primary"})
		require.NoError(t, err)
	}
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{MultiIndexer: mi, ConfigManager: cm, Graph: store, Logger: zap.NewNop()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Close() })
	srv := gortexmcp.NewServer(query.NewEngine(store), store, nil, nil, zap.NewNop(), nil, gortexmcp.MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm, CheckoutLifecycle: lifecycle})
	if state != "" {
		catalog := store.Catalog()
		require.NoError(t, catalog.UpsertRepositoryFamily(t.Context(), store_sqlite.RepositoryFamily{FamilyID: "family", CommonDirIdentity: filepath.Join(primary, ".git"), State: reconcile.FamilyStateReady}))
		for _, checkout := range []store_sqlite.Checkout{
			{CheckoutID: "primary", Incarnation: "primary-v1", FamilyID: "family", RootPath: primary, GitDir: filepath.Join(primary, ".git"), AdminName: "primary", State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated},
			{CheckoutID: "linked", Incarnation: "linked-v1", FamilyID: "family", RootPath: linked, GitDir: filepath.Join(primary, ".git", "worktrees", "linked"), AdminName: "linked", State: state, DesiredMode: store_sqlite.CheckoutModeAutomatic, EffectiveMode: store_sqlite.CheckoutModeAutomatic},
		} {
			require.NoError(t, catalog.UpsertCheckout(t.Context(), checkout))
		}
		require.NoError(t, catalog.UpsertDedicatedGraph(t.Context(), store_sqlite.DedicatedGraph{GraphID: indexer.GraphIDFor("primary"), OwnerCheckoutID: "primary", RepoPrefix: "primary", FamilyID: "family", IsPrimaryBase: true, State: reconcile.GraphStateReady}))
		srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: catalog})
	}
	return familyGuidanceFixture{newMCPDispatcher(srv, mi, zap.NewNop()), mi, cm.Global(), primary, linked}
}

func TestNotTrackedErrorFamilyMemberDiscoveryPending(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registered bool
		state      store_sqlite.CheckoutState
		finished   bool
		direct     bool
	}{
		{"unknown_catalog_family", true, "", false, false},
		{"configured_primary_unregistered_sibling", false, "", true, false},
		{"direct_configured_primary_unregistered", false, "", false, true},
		{"direct_primary_failed_registration", false, "", true, true},
		{"availability_grace", true, store_sqlite.CheckoutStateAvailabilityGrace, false, false},
		{"reconciling", true, store_sqlite.CheckoutStateReconciling, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFamilyGuidanceFixture(t, tc.registered, tc.state)
			if tc.finished {
				f.mi.ExpectRepos(f.config.ReposSnapshot())
				f.mi.FinishRepoLoad(f.primary, errors.New("fixture registration failed"))
			}
			cwd := f.linked
			if tc.direct {
				cwd = f.primary
			}
			sess := &daemon.Session{ID: tc.name, CWD: cwd}
			initialize := dispatchJSON(t, f.dispatcher, sess, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
			instructions := initialize["result"].(map[string]any)["instructions"].(string)
			require.NotContains(t, instructions, "gortex track "+cwd)
			require.Contains(t, instructions, "view_building")
			call := dispatchJSON(t, f.dispatcher, sess, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read","arguments":{"operation":"file","target":{"file":"fixture.go"}}}}`)
			data := errorData(t, call)
			require.Equal(t, "view_building", data["error_code"])
			require.Equal(t, "discovery_pending", data["state"])
			require.Equal(t, "family_pending", data["reason"])
			require.Equal(t, true, data["retryable"])
			if tc.direct && tc.finished {
				require.Equal(t, "failed", data["load_state"])
				require.Contains(t, data["load_reason"], "fixture registration failed")
				require.Contains(t, instructions, "fixture registration failed")
			}
			raw, err := json.Marshal(call)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "gortex track")
			outside := &daemon.Session{ID: "outside", CWD: t.TempDir()}
			require.Equal(t, "repo_not_tracked", errorCode(dispatchJSON(t, f.dispatcher, outside, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read","arguments":{}}}`)))
		})
	}
}

func TestBootstrapGateRefusesDirectFailedFamilyPrimary(t *testing.T) {
	f := newFamilyGuidanceFixture(t, false, "")
	f.mi.ExpectRepos(f.config.ReposSnapshot())
	f.mi.FinishRepoLoad(f.primary, errors.New("fixture registration failed"))
	before, err := os.ReadFile(f.config.ConfigPath())
	require.NoError(t, err)
	sess := &daemon.Session{ID: "direct-failed-primary", CWD: f.primary}
	for _, tool := range []string{"track_repository", "index_repository", "workspace_admin"} {
		args := map[string]any{"path": f.primary}
		if tool == "workspace_admin" {
			args = map[string]any{"operation": "track", "arguments": args}
		}
		frame, err := buildToolCallFrame(9, tool, args)
		require.NoError(t, err)
		data := errorData(t, dispatchJSON(t, f.dispatcher, sess, string(frame)))
		require.Equal(t, "view_building", data["error_code"])
		require.Equal(t, "family_member", data["reason"])
		require.Equal(t, "discovery_pending", data["state"])
		require.Contains(t, data["load_reason"], "fixture registration failed")
	}
	after, err := os.ReadFile(f.config.ConfigPath())
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Empty(t, f.mi.AllMetadata())
	unrelated := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(unrelated, "other.go"), []byte("package other\nfunc Other() {}\n"), 0o600))
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 10, "method": "tools/call", "params": map[string]any{"name": "workspace_admin", "arguments": map[string]any{"operation": "track", "arguments": map[string]any{"path": unrelated}}}})
	require.NoError(t, err)
	reply := dispatchJSON(t, f.dispatcher, sess, string(frame))
	require.Empty(t, errorCode(reply))
	require.Len(t, f.mi.AllMetadata(), 1)
	require.NotNil(t, f.config.FindRepoByPath(unrelated))
}

func TestDirectFamilyLoadingKeepsRepoLoadingPrecedence(t *testing.T) {
	f := newFamilyGuidanceFixture(t, false, "")
	f.mi.ExpectRepos(f.config.ReposSnapshot())
	sess := &daemon.Session{ID: "direct-loading", CWD: f.primary}
	call := dispatchJSON(t, f.dispatcher, sess, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read","arguments":{"operation":"file","target":{"file":"fixture.go"}}}}`)
	require.Equal(t, "repo_loading", errorData(t, call)["error_code"])
}

func TestBootstrapGateRefusesFamilyMemberTrack(t *testing.T) {
	f := newFamilyGuidanceFixture(t, true, "")
	sess := &daemon.Session{ID: "bootstrap", CWD: f.linked}
	before, err := os.ReadFile(f.config.ConfigPath())
	require.NoError(t, err)
	count := len(f.mi.AllMetadata())
	for _, name := range []string{"workspace_admin", "track_repository", "index_repository"} {
		args := map[string]any{"path": f.linked}
		if name == "workspace_admin" {
			args = map[string]any{"operation": "track", "arguments": args}
		}
		frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		require.NoError(t, err)
		data := errorData(t, dispatchJSON(t, f.dispatcher, sess, string(frame)))
		require.Equal(t, "view_building", data["error_code"])
		require.Equal(t, "discovery_pending", data["state"])
		require.Equal(t, "family_member", data["reason"])
	}
	after, err := os.ReadFile(f.config.ConfigPath())
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Len(t, f.mi.AllMetadata(), count)
	unrelated := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(unrelated, "other.go"), []byte("package other\nfunc Other() {}\n"), 0o600))
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": map[string]any{"name": "workspace_admin", "arguments": map[string]any{"operation": "track", "arguments": map[string]any{"path": unrelated}}}})
	require.NoError(t, err)
	reply := dispatchJSON(t, f.dispatcher, sess, string(frame))
	require.Empty(t, errorCode(reply))
	require.Contains(t, reply, "result")
	require.Len(t, f.mi.AllMetadata(), count+1)
	require.NotNil(t, f.config.FindRepoByPath(unrelated))
}
