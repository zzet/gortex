package indexer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestDurableMainPromotionRetryRecoversConfiguredPrefixAfterRollback(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := f.gitRepo("prefix-retry-root")
	const prefix = "stable-configured-prefix"
	gc := f.cm.Global()
	require.NoError(t, gc.AddRepo(config.RepoEntry{Path: root, Name: prefix}))
	require.NoError(t, gc.Save())

	// Reproduce configured startup up to transition admission without invoking
	// Seed's end-of-pass drain: one configured intent, one automatic checkout,
	// and one transient graph binding that the failed promotion must roll back.
	identity, err := f.lc.recordCheckout(ctx, prefix, root, TrackSourceConfig, true)
	require.NoError(t, err)
	require.NotEmpty(t, identity.checkoutID)
	require.Equal(t, GraphIDFor(prefix), identity.graphID)

	var moves atomic.Int32
	var barrierMu sync.Mutex
	var barrierErr error
	recordBarrierError := func(err error) {
		if err == nil {
			return
		}
		barrierMu.Lock()
		if barrierErr == nil {
			barrierErr = err
		}
		barrierMu.Unlock()
	}
	f.lc.indexBarrier = func() {
		move := moves.Add(1)
		content := fmt.Sprintf("package a\n\nfunc Moved%d() {}\n", move)
		if err := os.WriteFile(filepath.Join(root, "moved.go"), []byte(content), 0o644); err != nil {
			recordBarrierError(err)
			return
		}
		for _, args := range [][]string{
			{"add", "moved.go"},
			{"commit", "-q", "-m", fmt.Sprintf("move-%d", move)},
		} {
			cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
			cmd.Env = append(os.Environ(),
				"GIT_CONFIG_GLOBAL="+os.DevNull,
				"GIT_CONFIG_SYSTEM="+os.DevNull,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				recordBarrierError(fmt.Errorf("git %v: %w: %s", args, err, out))
				return
			}
		}
	}

	first, firstRun, err := f.lc.startPromoteCheckout(ctx, identity.checkoutID, TrackSourceImplicit)
	require.NoError(t, err)
	require.NotNil(t, firstRun)
	firstOutcome := waitPromotionRetryOutcome(t, firstRun)
	f.lc.indexBarrier = nil
	barrierMu.Lock()
	injectedErr := barrierErr
	barrierMu.Unlock()
	require.NoError(t, injectedErr)
	require.Error(t, firstOutcome.err)
	assert.ErrorIs(t, firstOutcome.err, ErrCheckoutMoved)
	assert.Equal(t, int32(2), moves.Load(), "both bounded full-tree attempts were invalidated")
	assert.True(t, firstOutcome.promotion.Pending)
	assert.True(t, firstOutcome.promotion.Retryable)

	_, bound, err := f.catalog.GetDedicatedGraph(ctx, GraphIDFor(prefix))
	require.NoError(t, err)
	assert.False(t, bound, "pre-publication rollback removes the transient graph")
	assert.Nil(t, f.mi.GetMetadata(prefix), "pre-publication rollback removes the transient repo shell")
	assert.Empty(t, f.mi.AllMetadata(), "rollback leaves no duplicate repository under another prefix")
	assert.True(t, f.configLists(root), "the durable configured authority survives rollback")
	require.Len(t, f.cm.RepoEntries(), 1)

	standing, found, err := f.catalog.GetIntentTransition(ctx, identity.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, first.TransitionID, standing.TransitionID)
	assert.Equal(t, store_sqlite.IntentTransitionPending, standing.State)

	// No Seed and no restart: the identical explicit demand adopts the durable
	// transition. At this point neither the catalog nor the process registry can
	// answer its prefix; only the canonical-root config lookup can self-heal it.
	retry, retryRun, err := f.lc.startPromoteCheckout(ctx, identity.checkoutID, TrackSourceImplicit)
	require.NoError(t, err)
	require.NotNil(t, retryRun)
	assert.Equal(t, first.TransitionID, retry.TransitionID)
	retryOutcome := waitPromotionRetryOutcome(t, retryRun)
	require.NoError(t, retryOutcome.err)
	result := retryOutcome.promotion
	assert.Equal(t, prefix, result.Prefix)
	assert.Equal(t, GraphIDFor(prefix), result.GraphID)

	graph, found, err := f.catalog.GetDedicatedGraph(ctx, result.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, identity.checkoutID, graph.OwnerCheckoutID)
	assert.Equal(t, prefix, graph.RepoPrefix)
	assert.Positive(t, graph.ActiveGenerationID)

	checkout, found, err := f.catalog.GetCheckout(ctx, identity.checkoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, store_sqlite.CheckoutModeDedicated, checkout.EffectiveMode)
	route, routed := f.routeOf(identity.checkoutID)
	require.True(t, routed)
	assert.Equal(t, result.GraphID, route.GraphID)
	assert.Positive(t, route.CommitGenerationID)
	assert.Positive(t, route.DirtyGenerationID)

	metadata := f.mi.AllMetadata()
	require.Len(t, metadata, 1, "retry restores exactly one process-local repository shell")
	require.NotNil(t, metadata[prefix])
	assert.Equal(t, pathkey.CanonicalExistingRoot(root),
		pathkey.CanonicalExistingRoot(metadata[prefix].RootPath))
	assert.Empty(t, contentIdentities(f.store, prefix), "dedicated shell carries no generation-zero duplicate")
	entries := f.cm.RepoEntries()
	require.Len(t, entries, 1, "retry does not duplicate the durable config entry")
	assert.Equal(t, prefix, config.ResolvePrefix(entries[0]))

	view := f.materialize(identity.checkoutID)
	identities := contentIdentities(view.Reader, prefix)
	view.Close()
	assert.Contains(t, identities, "prefix-retry-root.go::A")
	assert.Contains(t, identities, "moved.go::Moved2")
	assert.NotContains(t, identities, "moved.go::Moved1")
	_, found, err = f.catalog.GetIntentTransition(ctx, identity.checkoutID)
	require.NoError(t, err)
	assert.False(t, found, "successful retry releases the durable transition")

	intents, err := f.catalog.ListTrackingIntents(ctx, identity.checkoutID)
	require.NoError(t, err)
	require.Len(t, intents, 1)
	assert.Equal(t, TrackSourceConfig, intents[0].SourceKind)
}

func waitPromotionRetryOutcome(t *testing.T, run *modeTransitionRun) modeTransitionOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	outcome, err := waitModeTransition(ctx, run)
	require.NoError(t, err)
	return outcome
}

func BenchmarkConfiguredPromotionPrefixRetryLookup512(b *testing.B) {
	root := filepath.Join(b.TempDir(), "target")
	if err := os.MkdirAll(root, 0o755); err != nil {
		b.Fatal(err)
	}
	entries := make([]config.RepoEntry, 512)
	dummyRoot := b.TempDir()
	for i := range entries {
		entries[i] = config.RepoEntry{
			Path: filepath.Join(dummyRoot, fmt.Sprintf("repo-%03d", i)),
			Name: fmt.Sprintf("repo-%03d", i),
		}
	}
	entries[len(entries)-1] = config.RepoEntry{Path: root, Name: "target-prefix"}
	gc := &config.GlobalConfig{Repos: entries}
	configPath := filepath.Join(b.TempDir(), "config.yaml")
	gc.SetConfigPath(configPath)
	if err := gc.Save(); err != nil {
		b.Fatal(err)
	}
	manager, err := config.NewConfigManager(configPath)
	if err != nil {
		b.Fatal(err)
	}
	lifecycle := &CheckoutLifecycle{cfgMgr: manager}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := lifecycle.configuredPrefixForRoot(root); got != "target-prefix" {
			b.Fatalf("prefix = %q", got)
		}
	}
}
