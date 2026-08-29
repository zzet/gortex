package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// buildCatalogController builds a controller over a real sqlite store, which
// is what gives the lifecycle a checkout catalog to record identities in.
func buildCatalogController(t *testing.T) (*realController, *indexer.MultiIndexer, *store_sqlite.Catalog, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	store, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	mi := indexer.NewMultiIndexer(store, reg, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })

	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer:  mi,
		ConfigManager: cm,
		Graph:         store,
		Logger:        zap.NewNop(),
	})
	require.NoError(t, err)

	c := &realController{
		graph:         store,
		multiIndexer:  mi,
		configManager: cm,
		lifecycle:     lifecycle,
		logger:        zap.NewNop(),
	}
	watcher, err := indexer.NewMultiWatcher(mi, map[string]config.WatchConfig{}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, watcher.Start())
	c.AttachWatcher(watcher)
	t.Cleanup(func() {
		c.AttachWatcher(nil)
		require.NoError(t, watcher.Stop())
	})
	return c, mi, store.Catalog(), dir
}

// TestControllerTrackUntrackDrivesTheCheckoutLifecycle proves the control
// socket goes through the shared lifecycle: a track records the checkout
// under the CLI intent source, and an untrack revokes it and takes the
// identity, the corpus entry and the config line with it.
func TestControllerTrackUntrackDrivesTheCheckoutLifecycle(t *testing.T) {
	ctx := context.Background()
	c, mi, catalog, dir := buildCatalogController(t)

	repo := filepath.Join(dir, "cli-repo")
	wtGit(t, dir, "init", "-q", "-b", "main", "--", "cli-repo")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "lib.go"),
		[]byte("package lib\n\nfunc L() {}\n"), 0o644))
	wtGit(t, repo, "add", ".")
	wtGit(t, repo, "commit", "-q", "-m", "init")

	raw, err := c.Track(ctx, daemon.TrackParams{Path: repo})
	require.NoError(t, err)
	var tracked struct {
		Status string `json:"status"`
		Prefix string `json:"prefix"`
	}
	require.NoError(t, json.Unmarshal(raw, &tracked))
	require.Equal(t, "tracked", tracked.Status)
	require.NotNil(t, mi.GetMetadata(tracked.Prefix))

	binding, ok, err := catalog.GetDedicatedGraph(ctx, indexer.GraphIDFor(tracked.Prefix))
	require.NoError(t, err)
	require.True(t, ok, "the track recorded a dedicated-graph binding")
	intents, err := catalog.ListTrackingIntents(ctx, binding.OwnerCheckoutID)
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.Equal(t, store_sqlite.IntentSourceCLITrack, intents[0].SourceKind,
		"the control-socket call is what is recorded as having asked")

	// The wire verb carries the same preview-and-confirm gate as the tool
	// surface: a plan that removes rows is shown, not run, so an older CLI
	// binary — which sends nothing but a path — cannot escalate a request to
	// drop one checkout into a retirement of its family.
	raw, err = c.Untrack(ctx, daemon.UntrackParams{PathOrPrefix: repo})
	require.NoError(t, err)
	var previewed struct {
		Status          string `json:"status"`
		Plan            string `json:"plan"`
		ConfirmRequired bool   `json:"confirm_required"`
	}
	require.NoError(t, json.Unmarshal(raw, &previewed))
	assert.Equal(t, "preview", previewed.Status)
	assert.Equal(t, string(indexer.UntrackPlanPrimaryClosure), previewed.Plan)
	assert.True(t, previewed.ConfirmRequired)
	assert.NotNil(t, mi.GetMetadata(tracked.Prefix), "an unconfirmed untrack writes nothing")

	raw, err = c.Untrack(ctx, daemon.UntrackParams{PathOrPrefix: repo, Confirm: true})
	require.NoError(t, err)
	var untracked struct {
		Status  string   `json:"status"`
		Prefix  string   `json:"prefix"`
		Revoked []string `json:"revoked_intents"`
	}
	require.NoError(t, json.Unmarshal(raw, &untracked))
	assert.Equal(t, "untracked", untracked.Status)
	assert.Equal(t, tracked.Prefix, untracked.Prefix)
	assert.Equal(t, []string{string(store_sqlite.IntentSourceCLITrack)}, untracked.Revoked)

	assert.Nil(t, mi.GetMetadata(tracked.Prefix))
	checkouts, err := catalog.ListCheckouts(ctx, binding.FamilyID)
	require.NoError(t, err)
	assert.Empty(t, checkouts, "the forget saga removed the identity")

	saved, err := config.LoadGlobal(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	for _, entry := range saved.Repos {
		assert.NotEqual(t, repo, entry.Path, "the untrack is persisted")
	}
}

// TestControllerReloadDrivesTheCheckoutLifecycle pins the reload half of the
// control socket to the shared lifecycle. Dropping the only servable checkout
// from the config file must not delete its corpus: the removal is recorded as
// a pending transition and reported as one. A reload that evicted whatever
// left the config — the inline diff this converged on the lifecycle — passes
// every other reload test and fails here.
func TestControllerReloadDrivesTheCheckoutLifecycle(t *testing.T) {
	ctx := context.Background()
	c, mi, catalog, dir := buildCatalogController(t)

	repo := filepath.Join(dir, "reload-repo")
	wtGit(t, dir, "init", "-q", "-b", "main", "--", "reload-repo")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "lib.go"),
		[]byte("package lib\n\nfunc L() {}\n"), 0o644))
	wtGit(t, repo, "add", ".")
	wtGit(t, repo, "commit", "-q", "-m", "init")

	raw, err := c.Track(ctx, daemon.TrackParams{Path: repo, Name: "reload-repo"})
	require.NoError(t, err)
	var tracked struct {
		Prefix string `json:"prefix"`
	}
	require.NoError(t, json.Unmarshal(raw, &tracked))
	require.NotNil(t, mi.GetMetadata(tracked.Prefix))

	binding, ok, err := catalog.GetDedicatedGraph(ctx, indexer.GraphIDFor(tracked.Prefix))
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, binding.IsPrimaryBase, "the only checkout owns the family's primary base")

	// Nothing else can serve the family, so the entry leaving the config is
	// recorded rather than applied.
	require.NoError(t, c.configManager.Global().RemoveRepo(repo))
	require.NoError(t, c.configManager.Global().Save())

	pending := reloadCounts(t, c)
	assert.Equal(t, 1, pending.Pending, "the removal surfaces as a pending transition")
	assert.Equal(t, 0, pending.Removed)
	assert.NotNil(t, mi.GetMetadata(tracked.Prefix),
		"a config edit must not silently delete the only servable checkout")

	transition, ok, err := catalog.GetIntentTransition(ctx, binding.OwnerCheckoutID)
	require.NoError(t, err)
	require.True(t, ok, "the removal is durable, not just reported")
	assert.Equal(t, store_sqlite.IntentTransitionPending, transition.State)
}

// reloadPayload is what the controller's reload reports over the socket.
type reloadPayload struct {
	Added     int `json:"added"`
	Removed   int `json:"removed"`
	Pending   int `json:"pending"`
	Refreshed int `json:"refreshed"`
}

// reloadCounts drives the controller's reload and decodes what it reported.
func reloadCounts(t *testing.T, c *realController) reloadPayload {
	t.Helper()
	raw, err := c.Reload(context.Background())
	require.NoError(t, err)
	var out reloadPayload
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}
