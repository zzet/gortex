package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/testenv"
)

func startCatalogTrackDaemon(t *testing.T, entries, loaded []config.RepoEntry) (*config.ConfigManager, *indexer.MultiIndexer) {
	t.Helper()
	dir := testenv.ShortTempDir(t)
	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	gc.Repos = entries
	require.NoError(t, gc.Save())
	store, err := store_sqlite.Open(filepath.Join(dir, "catalog.sqlite"))
	require.NoError(t, err)
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cm, err := config.NewConfigManager(gc.ConfigPath())
	require.NoError(t, err)
	idx := indexer.New(store, reg, config.Default().Index, zap.NewNop())
	mi := indexer.NewMultiIndexer(store, reg, idx.Search(), cm, zap.NewNop())
	lc, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{MultiIndexer: mi, ConfigManager: cm, Graph: store, Logger: zap.NewNop()})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, lc.Close())
		require.NoError(t, mi.Close(context.Background()))
		idx.Close()
		require.NoError(t, store.Close())
	})
	for _, entry := range loaded {
		result, err := lc.Register(t.Context(), entry, indexer.TrackSourceCLI)
		require.NoError(t, err)
		require.NoError(t, result.CatalogErr)
	}
	srv := gortexmcp.NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil, gortexmcp.MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm})
	d := daemon.New(socket, "test", zap.NewNop())
	d.Controller = &realController{graph: store, multiIndexer: mi, configManager: cm, lifecycle: lc, logger: zap.NewNop()}
	d.MCPDispatcher = newMCPDispatcher(srv, mi, zap.NewNop())
	require.NoError(t, d.Listen())
	done := make(chan error, 1)
	go func() { done <- d.Serve() }()
	t.Cleanup(func() {
		require.NoError(t, d.Shutdown())
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	// Listen alone does not prove Serve has admitted its lifetime. Direct
	// controller tests can otherwise reach cleanup before that goroutine runs.
	client, err := daemon.DialToWithDeadline(socket, daemon.Handshake{Mode: daemon.ModeControl, ClientName: "catalog-test"}, time.Now().Add(2*time.Second))
	require.NoError(t, err)
	require.NoError(t, client.Close())
	return cm, mi
}

func TestRunTrackConfiguredNameChangeRequiresConfigEdit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	root := pathkey.CanonicalExistingRoot(t.TempDir())
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	gc.Repos = []config.RepoEntry{{Path: root}}
	require.NoError(t, gc.Save())
	oldName, oldWait, oldAs := trackName, trackWait, trackAsWorktree
	oldEnsure := trackEnsureDaemonReadyFn
	oldNotify := trackNotifyDaemonTrackFn
	t.Cleanup(func() {
		trackName, trackWait, trackAsWorktree = oldName, oldWait, oldAs
		trackEnsureDaemonReadyFn = oldEnsure
		trackNotifyDaemonTrackFn = oldNotify
	})
	trackName, trackWait, trackAsWorktree = "other-src", false, false
	trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonReady }
	trackNotifyDaemonTrackFn = func(string, string, time.Time) error { return nil }
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	err = runTrack(cmd, []string{root})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "global config")
	assert.Contains(t, err.Error(), "name:")
	reloaded, err := config.LoadGlobal()
	require.NoError(t, err)
	assert.Empty(t, reloaded.Repos[0].Name, "refusal must not pretend to persist a new name")
}

func TestRunUntrackConfiguredUnloadedReloads(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "separate", true: "nested"}[nested], func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			owner := pathkey.CanonicalExistingRoot(t.TempDir())
			other := pathkey.CanonicalExistingRoot(t.TempDir())
			if nested {
				other = filepath.Join(owner, "skipped")
				require.NoError(t, os.MkdirAll(other, 0o755))
			}
			entries := []config.RepoEntry{{Path: owner, Name: "src"}, {Path: other, Name: "src"}}
			cm, mi := startCatalogTrackDaemon(t, entries, entries[:1])
			cmd := &cobra.Command{}
			cmd.SetErr(&bytes.Buffer{})
			require.NoError(t, runUntrack(cmd, []string{other}))
			gc, err := config.LoadGlobal()
			require.NoError(t, err)
			require.Len(t, gc.Repos, 1)
			assert.Equal(t, owner, gc.Repos[0].Path)
			assert.Nil(t, cm.Global().FindRepoByPath(other), "fallback must reload the daemon's configured entries")
			require.NotNil(t, mi.GetMetadata("src"), "removing a skipped entry must keep the live owner")
			assert.Equal(t, owner, mi.GetMetadata("src").RootPath)
		})
	}
}

func TestRunTrackCatalogUsesDaemonWorktreeName(t *testing.T) {
	for _, scenario := range []string{"declared", "forced", "linked-primary", "bare"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			root := pathkey.CanonicalExistingRoot(t.TempDir())
			main := filepath.Join(root, "proj")
			require.NoError(t, os.MkdirAll(main, 0o755))
			git := func(dir string, args ...string) {
				t.Helper()
				out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
				require.NoError(t, err, "%s", out)
			}
			git(main, "init", "-q", "-b", "main")
			git(main, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init")
			tracked := main
			linked := filepath.Join(root, "declwt")
			want := "proj@declwt"
			if scenario == "forced" {
				linked = filepath.Join(root, "forcewt")
				want = "proj@forcewt"
			}
			if scenario == "linked-primary" || scenario == "bare" {
				if scenario == "bare" {
					bare := filepath.Join(root, "bare.git")
					git(root, "clone", "-q", "--bare", main, bare)
					main = bare
				}
				tracked = filepath.Join(root, "one", "src")
				git(main, "worktree", "add", "-q", "-b", "first", tracked)
				if scenario == "linked-primary" {
					require.NoError(t, os.WriteFile(filepath.Join(tracked, ".gortex.yaml"), []byte("workspace: team\n"), 0o644))
				}
				linked = filepath.Join(root, "two", "src")
				want = "src@src1"
				if scenario == "linked-primary" {
					want = "src@team@src1"
				}
			}
			git(main, "worktree", "add", "-q", "-b", "b-forced", linked)
			if scenario == "declared" || scenario == "linked-primary" {
				require.NoError(t, os.WriteFile(filepath.Join(linked, ".gortex.yaml"), []byte("workspace: team\n"), 0o644))
			}
			initial := []config.RepoEntry{{Path: tracked}}
			_, mi := startCatalogTrackDaemon(t, initial, initial)
			oldName, oldWait, oldAs := trackName, trackWait, trackAsWorktree
			oldEnsure := trackEnsureDaemonReadyFn
			t.Cleanup(func() {
				trackName, trackWait, trackAsWorktree = oldName, oldWait, oldAs
				trackEnsureDaemonReadyFn = oldEnsure
			})
			trackName, trackWait, trackAsWorktree = "", false, true // Test naming after explicit independent-graph opt-in.
			trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonReady }
			cmd := &cobra.Command{}
			cmd.SetErr(&bytes.Buffer{})
			require.NoError(t, runTrack(cmd, []string{linked}))
			meta := mi.GetMetadata(want)
			require.NotNil(t, meta, "CLI must use the same catalog-aware prefix as MCP")
			assert.Equal(t, linked, meta.RootPath)
			gc, err := config.LoadGlobal()
			require.NoError(t, err)
			entry := gc.FindRepoByPath(linked)
			require.NotNil(t, entry)
			assert.Equal(t, want, entry.Name, "the daemon's choice must survive restart")
		})
	}
}

func TestTrackPrefixCollisionSilencesUsage(t *testing.T) {
	assert.True(t, trackCmd.SilenceUsage)
}

type untrackProbeController struct {
	*realController
	answer daemon.ProbeResponse
}

func (c *untrackProbeController) Probe(context.Context) (daemon.ProbeResponse, error) {
	return c.answer, nil
}

func TestUntrackConfiguredRootNeedsLoadedProof(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := pathkey.CanonicalExistingRoot(t.TempDir())
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	gc.Repos = []config.RepoEntry{{Path: root}}
	require.NoError(t, gc.Save())
	before, err := os.ReadFile(gc.ConfigPath())
	require.NoError(t, err)
	dir := testenv.ShortTempDir(t)
	socket := filepath.Join(dir, "s")
	t.Setenv("GORTEX_DAEMON_SOCKET", socket)
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(dir, "p"))
	d := daemon.New(socket, "test", zap.NewNop())
	d.Controller = &untrackProbeController{realController: &realController{}, answer: daemon.ProbeResponse{TrackedRepos: []daemon.ProbeRepo{{Path: root}}}}
	require.NoError(t, d.Listen())
	done := make(chan error, 1)
	go func() { done <- d.Serve() }()
	t.Cleanup(func() {
		require.NoError(t, d.Shutdown())
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	client, err := daemon.DialToWithDeadline(socket, daemon.Handshake{Mode: daemon.ModeControl, ClientName: "catalog-test"}, time.Now().Add(2*time.Second))
	require.NoError(t, err)
	require.NoError(t, client.Close())
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	err = runUntrack(cmd, []string{root})
	require.ErrorContains(t, err, "cannot verify loaded roots")
	after, err := os.ReadFile(gc.ConfigPath())
	require.NoError(t, err)
	assert.Equal(t, before, after, "unknown loaded state must not bypass untrack preview")
}

func TestRunTrackConfiguredUnloadedKeepsName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := pathkey.CanonicalExistingRoot(t.TempDir())
	entries := []config.RepoEntry{{Path: root, Name: "mylib"}}
	_, mi := startCatalogTrackDaemon(t, entries, nil)
	oldName, oldWait, oldEnsure := trackName, trackWait, trackEnsureDaemonReadyFn
	t.Cleanup(func() { trackName, trackWait, trackEnsureDaemonReadyFn = oldName, oldWait, oldEnsure })
	trackName, trackWait = "", false
	trackEnsureDaemonReadyFn = func(bool) daemonDecision { return daemonReady }
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetErr(&output)
	require.NoError(t, runTrack(cmd, []string{root}))
	require.NotNil(t, mi.GetMetadata("mylib"), "configured name must be sent even before warmup loads it")
	assert.Contains(t, output.String(), "mylib")
	gc, err := config.LoadGlobal()
	require.NoError(t, err)
	assert.Equal(t, "mylib", gc.FindRepoByPath(root).Name)
}

func TestControllerTrackConfiguredNameChangeRequiresConfigEdit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := pathkey.CanonicalExistingRoot(t.TempDir())
	entries := []config.RepoEntry{{Path: root, Name: "src"}}
	cm, mi := startCatalogTrackDaemon(t, entries, nil)
	lc, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{MultiIndexer: mi, ConfigManager: cm, Logger: zap.NewNop()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lc.Close()) })
	c := &realController{multiIndexer: mi, configManager: cm, lifecycle: lc, logger: zap.NewNop()}
	_, err = c.Track(t.Context(), daemon.TrackParams{Path: root, Name: "other-src"})
	require.ErrorContains(t, err, "already configured; set name: other-src")
	assert.Empty(t, mi.AllMetadata(), "refusal must precede indexing")
	assert.Equal(t, "src", cm.Global().FindRepoByPath(root).Name)
}

func TestConfiguredWorktreePrefixInventoriesOnce(t *testing.T) {
	root := pathkey.CanonicalExistingRoot(t.TempDir())
	main, linked := filepath.Join(root, "proj"), filepath.Join(root, "linked")
	require.NoError(t, os.MkdirAll(main, 0o755))
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", main}, args...)...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	git("init", "-q", "-b", "main")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init")
	git("worktree", "add", "-q", "-b", "linked", linked)
	old := trackInventoryFn
	t.Cleanup(func() { trackInventoryFn = old })
	calls := 0
	trackInventoryFn = func(ctx context.Context, path string) (*gitstate.FamilyInventory, error) {
		calls++
		return gitstate.Inventory(ctx, path)
	}
	for _, configured := range []bool{false, true} {
		calls = 0
		entries := []config.RepoEntry{{Path: t.TempDir()}, {Path: t.TempDir()}}
		if configured {
			entries = append(entries, config.RepoEntry{Path: main})
		}
		got := configuredWorktreePrefix(linked, entries, nil)
		if configured {
			require.Equal(t, "proj@linked", got)
		} else {
			require.Empty(t, got)
		}
		require.Equal(t, 1, calls, "inventory is per command, not per configured entry")
	}
}

func TestConfiguredWorktreePrefixMatchesCaseAliases(t *testing.T) {
	root := pathkey.CanonicalExistingRoot(t.TempDir())
	main, linked := filepath.Join(root, "Family"), filepath.Join(root, "linked")
	require.NoError(t, os.Mkdir(main, 0o755))
	alias := filepath.Join(root, "family")
	info, err := os.Stat(alias)
	if os.IsNotExist(err) {
		t.Skip("filesystem is case-sensitive")
	}
	require.NoError(t, err)
	original, err := os.Stat(main)
	require.NoError(t, err)
	require.True(t, os.SameFile(original, info))
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", main}, args...)...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	}
	git("init", "-q", "-b", "main")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init")
	git("worktree", "add", "-q", "-b", "linked", linked)
	got := configuredWorktreePrefix(linked, []config.RepoEntry{{Path: alias, Name: "chosen"}}, nil)
	assert.Equal(t, "chosen@linked", got, "configured case aliases must retain the family's prefix")
}
