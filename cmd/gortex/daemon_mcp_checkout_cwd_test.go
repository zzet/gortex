package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
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

// checkoutCWDSetup builds a dispatcher whose server knows one tracked
// repository and one registered automatic checkout of its family — the shape
// a git worktree of a tracked repo takes. The worktree root is deliberately
// OUTSIDE the tracked root: that is what makes it invisible to the repository
// registry and visible only to the view catalog. When loading is set, the
// persisted catalog is available before the primary registers.
func checkoutCWDSetup(t *testing.T, loading bool) (d *mcpDispatcher, primaryRoot, worktreeRoot string) {
	t.Helper()
	dir := t.TempDir()
	primaryRoot = filepath.Join(dir, "repos", "app")
	worktreeRoot = filepath.Join(dir, "worktrees", "feature")

	store, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(store, reg, config.Default().Index, zap.NewNop())
	mi := indexer.NewMultiIndexer(store, reg, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })

	ctx := context.Background()
	require.NoError(t, os.MkdirAll(primaryRoot, 0o755))
	entry := config.RepoEntry{Path: primaryRoot}
	prefix := config.ResolvePrefix(entry)
	if loading {
		discoveryScopeRepo(t, primaryRoot)
		require.NoError(t, os.MkdirAll(filepath.Dir(worktreeRoot), 0o755))
		guidanceGit(t, primaryRoot, "worktree", "add", "-q", "-b", "feature", worktreeRoot)
		mi.ExpectRepos([]config.RepoEntry{entry})
		mi.StartRepoLoad(primaryRoot)
	} else {
		require.NoError(t, os.MkdirAll(worktreeRoot, 0o755))
		res, err := mi.TrackRepoCtx(ctx, entry)
		require.NoError(t, err)
		prefix = res.RepoPrefix
	}

	// The corpus has to carry the prefix: the family list a view lookup walks
	// is derived from the repo prefixes the indexed graph holds.
	store.AddBatch([]*graph.Node{{
		ID:         prefix + "/app.go::Run",
		Kind:       graph.KindFunction,
		Name:       "Run",
		FilePath:   prefix + "/app.go",
		RepoPrefix: prefix,
		Language:   "go",
		StartLine:  1,
		EndLine:    3,
	}}, nil)

	catalog := store.Catalog()
	const familyID = "family-checkout-cwd"
	require.NoError(t, catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: filepath.Join(primaryRoot, ".git"),
		State:             reconcile.FamilyStateReady,
		CreatedAt:         100,
		LastSeen:          100,
	}))
	require.NoError(t, catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    "chk-primary",
		Incarnation:   "inc-primary",
		FamilyID:      familyID,
		RootPath:      primaryRoot,
		GitDir:        filepath.Join(primaryRoot, ".git"),
		AdminName:     "primary",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      101,
	}))
	require.NoError(t, catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    "chk-worktree",
		Incarnation:   "inc-worktree",
		FamilyID:      familyID,
		RootPath:      worktreeRoot,
		GitDir:        filepath.Join(primaryRoot, ".git", "worktrees", "feature"),
		AdminName:     "feature",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		LastSeen:      101,
	}))
	require.NoError(t, catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         indexer.GraphIDFor(prefix),
		OwnerCheckoutID: "chk-primary",
		RepoPrefix:      prefix,
		FamilyID:        familyID,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	}))

	srv := gortexmcp.NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil,
		gortexmcp.MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm})
	srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: catalog})

	return newMCPDispatcher(srv, mi, zap.NewNop()), primaryRoot, worktreeRoot
}

// TestDispatcher_RegisteredCheckoutCWDPasses covers the session cwd that lies
// inside no tracked repo and contains none, but that the view catalog knows as
// an automatic checkout of a tracked family.
//
// sessionScope binds exactly this cwd to the family's primary, so the gate in
// front of it has to agree: refusing here left every worktree session with the
// repo_not_tracked refusal and a remedy that would have indexed the worktree
// as a second repository.
func TestDispatcher_RegisteredCheckoutCWDPasses(t *testing.T) {
	ctx := context.Background()
	d, _, worktree := checkoutCWDSetup(t, false)

	assert.True(t, d.cwdReachable(ctx, worktree),
		"a cwd inside a registered automatic checkout must be reachable")

	sess := &daemon.Session{ID: "sess_checkout", CWD: worktree}
	frame := []byte(`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"graph_stats","arguments":{}}}`)
	reply, err := d.Dispatch(ctx, sess, frame)
	require.NoError(t, err)
	require.NotNil(t, reply)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(reply, &parsed))
	if errObj, ok := parsed["error"].(map[string]any); ok {
		if data, ok := errObj["data"].(map[string]any); ok {
			assert.NotEqual(t, "repo_not_tracked", data["error_code"],
				"registered-checkout cwd wrongly rejected by the gate: %v", parsed)
		}
	}
}

// TestDispatcher_UnregisteredWorktreeCWDStillRejected guards the widening. A
// directory the view catalog does not know stays refused — the catalog arm
// admits registered checkouts, not every path that happens to sit beside one.
func TestDispatcher_UnregisteredWorktreeCWDStillRejected(t *testing.T) {
	ctx := context.Background()
	d, _, worktree := checkoutCWDSetup(t, false)

	stranger := filepath.Join(filepath.Dir(worktree), "not-registered")
	assert.False(t, d.cwdReachable(ctx, stranger),
		"a sibling directory no checkout owns must stay unreachable")
}

func TestDispatcher_LoadingLinkedCheckoutFacadeCatalog(t *testing.T) {
	d, primary, worktree := checkoutCWDSetup(t, true)
	primary, err := filepath.EvalSymlinks(primary)
	require.NoError(t, err)
	sess := &daemon.Session{ID: "loading-linked-catalog", CWD: worktree, ToolSpec: "facade-v1", ToolMode: "hide"}
	d.SessionStarted(sess, func([]byte) error { return nil })
	t.Cleanup(func() { d.SessionEnded(sess) })
	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"loading-linked-catalog","version":"1"}}}`
	require.Contains(t, dispatchJSON(t, d, sess, initialize), "result")

	graph := dispatchJSON(t, d, sess, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"workspace","arguments":{"operation":"graph"}}}`)
	refusal := errorData(t, graph)
	require.Equal(t, "repo_loading", refusal["error_code"])
	require.Equal(t, true, refusal["retryable"])
	require.Equal(t, primary, refusal["repo_path"])
	require.Equal(t, string(indexer.RepoLoadRunning), refusal["state"])

	for _, args := range []string{"null", "{}"} {
		t.Run("catalog "+args, func(t *testing.T) {
			reply := dispatchJSON(t, d, sess, catalogCallFrame(args))
			result := toolResultText(t, reply)
			if assert.NotEqual(t, true, reply["result"].(map[string]any)["isError"], "%s", result) {
				var catalog struct {
					Surface string           `json:"surface_version"`
					Domains []map[string]any `json:"domains"`
				}
				require.NoError(t, json.Unmarshal([]byte(result), &catalog))
				assert.Equal(t, gortexmcp.FacadeSurfaceVersion, catalog.Surface)
				assert.NotEmpty(t, catalog.Domains)
				assert.NotContains(t, result, "chk-worktree")
			}
		})
	}
	for name, args := range map[string]string{
		"explicit automatic view": `{"view":{"kind":"auto"}}`,
		"explicit checkout view":  `{"view":{"kind":"worktree","checkout_id":"chk-worktree"}}`,
		"exact view":              `{"require_exact":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			reply := dispatchJSON(t, d, sess, catalogCallFrame(args))
			assert.Contains(t, toolResultText(t, reply), "selector_out_of_scope")
			assert.Equal(t, true, reply["result"].(map[string]any)["isError"])
		})
	}
}

func catalogCallFrame(args string) string {
	return `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"capabilities","arguments":` + args + `}}`
}
