package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

// gateHoldingReindexStore wraps a real SQLite store and holds its real writer
// gate across the first ReindexEdgesApplied call, releasing it as soon as the
// timed-out rebind returns false so subsequent digest writes see a free gate.
type gateHoldingReindexStore struct {
	*store_sqlite.Store
	holdOnce     sync.Once
	dropped      atomic.Int32
	droppedEdges atomic.Int32
}

func (s *gateHoldingReindexStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.ReindexEdgesApplied(batch)
}

func (s *gateHoldingReindexStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	return s.ReindexEdgesTryApplied(batch, 0)
}

func (s *gateHoldingReindexStore) ReindexEdgesTryApplied(batch []graph.EdgeReindex, maxWait time.Duration) bool {
	var release func()
	s.holdOnce.Do(func() {
		var err error
		release, err = s.HoldWriteGate(context.Background())
		if err != nil {
			panic(err)
		}
	})
	if release != nil {
		defer release()
	}
	applied := s.Store.ReindexEdgesTryApplied(batch, maxWait)
	if !applied {
		s.dropped.Add(1)
		s.droppedEdges.Add(int32(len(batch)))
	}
	return applied
}

// failingResolveStore fails a whole-graph resolve at its pending-edge read.
type failingResolveStore struct {
	graph.Store
	err error
}

func (s failingResolveStore) BeginUnresolvedEdgeScan(context.Context) (graph.UnresolvedEdgeScan, error) {
	return graph.UnresolvedEdgeScan{}, s.err
}

func (s failingResolveStore) ReadUnresolvedEdgePage(context.Context, graph.UnresolvedEdgeScan, int64, int, int) (graph.UnresolvedEdgePage, error) {
	return graph.UnresolvedEdgePage{}, s.err
}

// Both targets declare the same symbol, so name-only fallback cannot stand
// in for an alias-directed binding.
func aliasRebindFixture(t *testing.T, root string) string {
	t.Helper()
	configPath := filepath.Join(root, "tsconfig.json")
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["old/*"]}}}`)
	writeFileMk(t, filepath.Join(root, "old/auth.ts"), "export function useRedirect() { return 1 }\n")
	writeFileMk(t, filepath.Join(root, "new/auth.ts"), "export function useRedirect() { return 2 }\n")
	writeFileMk(t, filepath.Join(root, "pages/index.ts"), "import { useRedirect } from '~/auth'\nexport function run() { return useRedirect() }\n")
	return configPath
}

func TestTSConfigPathAlias_ConfigChangeOnFullReindex(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"), "same-root reindex must not keep the old config")
}

func TestTSConfigPathAlias_NewConfigAfterNegativeFullScan(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, "lib/auth.ts"), "export function useRedirect() { return 1 }\n")
	writeFileMk(t, filepath.Join(root, "pages/index.ts"), "import { useRedirect } from '~/auth'\nexport function run() { return useRedirect() }\n")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Nil(t, loadTSAliasCollection(root), "absence of configs is cached too")
	writeFileMk(t, filepath.Join(root, "tsconfig.json"), `{"compilerOptions":{"paths":{"~/*":["lib/*"]}}}`)
	idx, _ = newSQLiteIndexer(t)
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "lib/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestTSConfigPathAlias_ConfigChangeOnIncrementalReindex(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	caller := filepath.Join(root, "pages/index.ts")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	bumpMtime(t, caller, "import { useRedirect } from '~/auth'\nexport function run() { useRedirect(); return useRedirect() }\n")
	_, err = idx.IncrementalReindexPaths(root, []string{caller})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestTSConfigPathAlias_ConfigOnlyIncrementalReindex(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	_, err = idx.IncrementalReindexPaths(root, []string{configPath})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestTSConfigPathAlias_CancelledRebindKeepsGraphAndRetries(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)

	budget := newIndexMemoryAdmissionBudget(1)
	held, err := budget.acquire(t.Context(), 1)
	require.NoError(t, err)
	defer held.Release()
	idx.indexMemoryAdmission = budget
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := idx.IndexCtx(ctx, root)
		done <- err
	}()
	waitForIndexMemoryWaiters(t, budget, 1)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))

	// The lookup cache has already seen the new config. The graph has not.
	_, err = idx.IncrementalReindexPaths(root, []string{configPath})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestTSConfigPathAlias_FailedFullResolveIsNotCertified(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	oldDigest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.NotEmpty(t, oldDigest)

	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	errResolve := errors.New("injected pending page failure")
	healthy := idx.resolver
	idx.resolver = resolver.New(failingResolveStore{Store: store, err: errResolve})
	_, err = idx.Index(root)
	idx.resolver = healthy
	require.ErrorIs(t, err, errResolve)
	digest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.Empty(t, digest, "a failed resolve must not certify either policy")
	require.Empty(t, idx.appliedAliases)

	_, err = idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	digest, err = store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.NotEmpty(t, digest)
	require.NotEqual(t, oldDigest, digest)
}

func TestTSConfigPathAlias_DeferredIndexCertifiesAfterSharedResolve(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	entry := config.RepoEntry{Path: root, Name: "repo"}
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{entry}}
	gc.SetConfigPath(configFile)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(configFile)
	require.NoError(t, err)
	seed, store := newSQLiteIndexer(t)
	invalidateTSAliasCollection(root)
	mi := NewMultiIndexer(store, seed.registry, search.NewNull(), cm, zap.NewNop())
	_, err = mi.TrackRepoCtx(t.Context(), entry)
	require.NoError(t, err)
	require.Equal(t, "repo/old/auth.ts::useRedirect", callTargetForName(t, store, "repo/pages/index.ts", "run"))

	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	idx := mi.GetIndexer("repo")
	idx.SetDeferResolve(true)
	_, err = idx.IndexCtx(t.Context(), root)
	require.NoError(t, err)
	digest, err := store.TSAliasDigest("repo")
	require.NoError(t, err)
	require.Empty(t, digest, "the shared resolve has not run yet")

	errResolve := errors.New("injected pending page failure")
	mi.graph = failingResolveStore{Store: store, err: errResolve}
	require.ErrorIs(t, mi.RunPreEnrichResolve(t.Context(), nil, nil), errResolve)
	mi.graph = store
	digest, err = store.TSAliasDigest("repo")
	require.NoError(t, err)
	require.Empty(t, digest, "a failed shared resolve must not certify the policy")

	require.NoError(t, mi.RunPreEnrichResolve(t.Context(), nil, nil))
	require.Equal(t, "repo/new/auth.ts::useRedirect", callTargetForName(t, store, "repo/pages/index.ts", "run"))
	digest, err = store.TSAliasDigest("repo")
	require.NoError(t, err)
	require.NotEmpty(t, digest)
	require.Nil(t, idx.deferredAliases.Load())
}

func TestTSConfigPathAlias_FailedCatchupResolveIsNotCertified(t *testing.T) {
	idx, store := newSQLiteIndexer(t)
	idx.passAliases.Store(&aliasSnapshot{digest: "new"})
	errResolve := errors.New("injected catch-up failure")
	require.ErrorIs(t, idx.finishAliasPass(&IndexResult{resolutionErr: errResolve}), errResolve)
	digest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.Empty(t, digest)
	require.Empty(t, idx.appliedAliases)
}

func TestTSConfigPathAlias_RebindPreservesExplicitScope(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	goFile := filepath.Join(root, "keep.go")
	unrelated := filepath.Join(root, "other.go")
	writeFileMk(t, goFile, "package p\nfunc Before() {}\n")
	writeFileMk(t, unrelated, "package p\nfunc Other() {}\n")
	idx, _ := newSQLiteIndexer(t)
	idx.config.Merkle = false
	_, err := idx.Index(root)
	require.NoError(t, err)
	stamp, err := os.Stat(goFile)
	require.NoError(t, err)
	writeFileMk(t, goFile, "package p\nfunc After() {}\n")
	require.NoError(t, os.Chtimes(goFile, stamp.ModTime(), stamp.ModTime()))
	require.NoError(t, os.Remove(unrelated))
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	require.NoError(t, idx.IndexFile(goFile))
	require.NotEmpty(t, fnNodeID(t, idx.Graph(), "keep.go", "After"))
	require.NotEmpty(t, fnNodeID(t, idx.Graph(), "other.go", "Other"))
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestTSConfigPathAlias_WarmSaveKeepsBoundedProof(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	proof := newBuildContentProof(root, gitstate.DirtySnapshot{})
	idx.contentProof = proof
	_, err = idx.IncrementalReindexPaths(root, []string{"pages/index.ts"})
	require.NoError(t, err)
	require.False(t, proof.isUnbounded())
	set, _ := buildReadSetFrom(proof.readSet(t.Context(), nil, nil))
	require.True(t, readSetHolds(set, filepath.Base(configPath)))
}

// A file the alias rebind cannot read is retried from the failure ledger.
// It must not keep every later save restaging all JS/TS callers.
func TestTSConfigPathAlias_FailedFileDoesNotWidenLaterSaves(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	locked := filepath.Join(root, "pages/locked.ts")
	writeFileMk(t, locked, "import { useRedirect } from '~/auth'\nexport function other() { return useRedirect() }\n")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	restore := denyFileRead(t, locked)
	result, err := idx.IncrementalReindexPaths(root, []string{configPath})
	require.NoError(t, err)
	require.Contains(t, result.FailedFiles, locked)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))

	caller := filepath.Join(root, "pages/index.ts")
	bumpMtime(t, caller, "import { useRedirect } from '~/auth'\nexport function run() { useRedirect(); return useRedirect() }\n")
	result, err = idx.IncrementalReindexPaths(root, []string{caller})
	require.NoError(t, err)
	require.Equal(t, 1, result.StaleFileCount, "a save must restage only itself")

	restore()
	_, err = idx.IncrementalReindexPaths(root, []string{locked})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/locked.ts", "other"))
}

func TestTSConfigPathAlias_RefusedFullResolveWriteIsNotCertified(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))
	oldDigest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.NotEmpty(t, oldDigest)

	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	restoreTimeout := store.SetBusyRetryTimeoutForTest(40 * time.Millisecond)
	defer restoreTimeout()
	contended := &gateHoldingReindexStore{Store: store}
	idx.resolver.SetGraph(contended)
	res, err := idx.Index(root)
	idx.resolver.SetGraph(store)
	require.NoError(t, err, "a refused resolver write during full index must still publish the graph")
	require.NotNil(t, res)
	require.True(t, res.ResolutionWriteRefused)
	require.Equal(t, int32(1), contended.dropped.Load(), "SQLite must have dropped the contended rebind batch")
	require.Positive(t, contended.droppedEdges.Load())
	require.Equal(t, "unresolved::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))
	digest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.Empty(t, digest, "a refused resolver write must not certify the new alias digest")
	require.Empty(t, idx.appliedAliases)

	res, err = idx.Index(root)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.ResolutionWriteRefused)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))
	digest, err = store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.NotEmpty(t, digest)
	require.NotEqual(t, oldDigest, digest)
}

func TestTSConfigPathAlias_RefusedIncrementalResolveWriteIsNotCertified(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	oldDigest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.NotEmpty(t, oldDigest)

	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	restoreTimeout := store.SetBusyRetryTimeoutForTest(40 * time.Millisecond)
	defer restoreTimeout()
	contended := &gateHoldingReindexStore{Store: store}
	idx.resolver.SetGraph(contended)
	_, err = idx.IncrementalReindexPaths(root, []string{configPath})
	idx.resolver.SetGraph(store)
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	require.Equal(t, int32(1), contended.dropped.Load())
	require.Equal(t, "unresolved::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))
	digest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.Empty(t, digest, "a refused incremental rebind write must not certify the new alias digest")
	require.Empty(t, idx.appliedAliases)

	_, err = idx.IncrementalReindexPaths(root, []string{configPath})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))
	digest, err = store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.NotEmpty(t, digest)
	require.NotEqual(t, oldDigest, digest)
}

func TestTSConfigPathAlias_RefusedDeferredSharedResolveWriteIsNotCertified(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	entry := config.RepoEntry{Path: root, Name: "repo"}
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{entry}}
	gc.SetConfigPath(configFile)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(configFile)
	require.NoError(t, err)
	seed, store := newSQLiteIndexer(t)
	invalidateTSAliasCollection(root)
	mi := NewMultiIndexer(store, seed.registry, search.NewNull(), cm, zap.NewNop())
	_, err = mi.TrackRepoCtx(t.Context(), entry)
	require.NoError(t, err)
	require.Equal(t, "repo/old/auth.ts::useRedirect", callTargetForName(t, store, "repo/pages/index.ts", "run"))

	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	idx := mi.GetIndexer("repo")
	idx.SetDeferResolve(true)
	_, err = idx.IndexCtx(t.Context(), root)
	require.NoError(t, err)

	restoreTimeout := store.SetBusyRetryTimeoutForTest(40 * time.Millisecond)
	defer restoreTimeout()
	contended := &gateHoldingReindexStore{Store: store}
	mi.graph = contended
	err = mi.RunPreEnrichResolve(t.Context(), nil, nil)
	mi.graph = store
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	require.Equal(t, int32(1), contended.dropped.Load())
	require.Equal(t, "unresolved::useRedirect", callTargetForName(t, store, "repo/pages/index.ts", "run"))
	digest, err := store.TSAliasDigest("repo")
	require.NoError(t, err)
	require.Empty(t, digest, "a refused shared resolve write must not certify the deferred alias policy")
	require.NotNil(t, idx.deferredAliases.Load())

	require.NoError(t, mi.RunPreEnrichResolve(t.Context(), nil, nil))
	require.Equal(t, "repo/new/auth.ts::useRedirect", callTargetForName(t, store, "repo/pages/index.ts", "run"))
	digest, err = store.TSAliasDigest("repo")
	require.NoError(t, err)
	require.NotEmpty(t, digest)
	require.Nil(t, idx.deferredAliases.Load())
}

func TestTSConfigPathAlias_TrackRepoPublishesMetadataOnRefusedFullResolveWrite(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	root := t.TempDir()
	aliasRebindFixture(t, root)
	entry := config.RepoEntry{Path: root, Name: "repo"}
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{entry}}
	gc.SetConfigPath(configFile)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(configFile)
	require.NoError(t, err)
	seed, store := newSQLiteIndexer(t)
	restoreTimeout := store.SetBusyRetryTimeoutForTest(40 * time.Millisecond)
	defer restoreTimeout()
	contended := &gateHoldingReindexStore{Store: store}
	invalidateTSAliasCollection(root)
	mi := NewMultiIndexer(contended, seed.registry, search.NewNull(), cm, zap.NewNop())

	res, err := mi.TrackRepoCtx(t.Context(), entry)
	require.NoError(t, err, "TrackRepoCtx must complete and publish metadata when resolution hits a refused write")
	require.NotNil(t, res)
	require.True(t, res.ResolutionWriteRefused)
	require.Equal(t, int32(1), contended.dropped.Load())
	digest, err := store.TSAliasDigest("repo")
	require.NoError(t, err)
	require.Empty(t, digest, "alias digest must be withheld when a full resolve write was refused")
	meta := mi.GetMetadata("repo")
	require.NotNil(t, meta, "RepoMetadata must be published on refused resolve write")
	require.Positive(t, meta.FileCount)
	require.NotEmpty(t, mi.FileMtimes("repo"), "FileMtimes must be published on refused resolve write")
}

func TestTSConfigPathAlias_FullIndexFailedTSFileDoesNotWidenLaterSaves(t *testing.T) {
	root := t.TempDir()
	aliasRebindFixture(t, root)
	locked := filepath.Join(root, "pages/locked.ts")
	writeFileMk(t, locked, "import { useRedirect } from '~/auth'\nexport function other() { return useRedirect() }\n")
	restore := denyFileRead(t, locked)

	idx, store := newSQLiteIndexer(t)
	result, err := idx.Index(root)
	require.NoError(t, err)
	require.Contains(t, result.FailedFiles, locked)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))
	digest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.NotEmpty(t, digest, "retryable file failure during full index must still certify the alias policy")

	caller := filepath.Join(root, "pages/index.ts")
	bumpMtime(t, caller, "import { useRedirect } from '~/auth'\nexport function run() { useRedirect(); return useRedirect() }\n")
	result, err = idx.IncrementalReindexPaths(root, []string{caller})
	require.NoError(t, err)
	require.Equal(t, 1, result.StaleFileCount, "a save must restage only itself")

	restore()
	_, err = idx.IncrementalReindexPaths(root, []string{locked})
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, store, "pages/locked.ts", "other"))
}

func TestTSConfigPathAlias_FullIndexFailedGoFileInMixedRepoDoesNotWidenLaterSaves(t *testing.T) {
	root := t.TempDir()
	aliasRebindFixture(t, root)
	lockedGo := filepath.Join(root, "broken.go")
	writeFileMk(t, lockedGo, "package p\nfunc Helper() {}\n")
	restore := denyFileRead(t, lockedGo)

	idx, store := newSQLiteIndexer(t)
	result, err := idx.Index(root)
	require.NoError(t, err)
	require.Contains(t, result.FailedFiles, lockedGo)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))
	digest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.NotEmpty(t, digest, "a retryable non-JS/TS file failure during full index must still certify the alias policy")

	caller := filepath.Join(root, "pages/index.ts")
	bumpMtime(t, caller, "import { useRedirect } from '~/auth'\nexport function run() { useRedirect(); return useRedirect() }\n")
	result, err = idx.IncrementalReindexPaths(root, []string{caller})
	require.NoError(t, err)
	require.Equal(t, 1, result.StaleFileCount, "a save must restage only itself")

	restore()
	_, err = idx.IncrementalReindexPaths(root, []string{lockedGo})
	require.NoError(t, err)
	require.NotEmpty(t, fnNodeID(t, store, "broken.go", "Helper"))
}

func TestIndexerResolveAllRunsDerivedPassesOnRefusedWrite(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, "iface.go"), `package p
type Runner interface {
	Run()
}
type Worker struct{}
func (Worker) Run() {}
func invoke() { missingHelper() }
func missingHelper() {}
`)
	idx, store := newSQLiteIndexer(t)
	idx.SetDeferResolve(true)
	_, err := idx.Index(root)
	require.NoError(t, err)
	idx.SetDeferResolve(false)

	restoreTimeout := store.SetBusyRetryTimeoutForTest(40 * time.Millisecond)
	defer restoreTimeout()
	contended := &gateHoldingReindexStore{Store: store}
	idx.resolver.SetGraph(contended)
	err = idx.resolveAll()
	idx.resolver.SetGraph(store)
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	require.Equal(t, int32(1), contended.dropped.Load())

	var sawImplements bool
	for e := range store.EdgesByKind(graph.EdgeImplements) {
		if e.From == "iface.go::Worker" && e.To == "iface.go::Runner" {
			sawImplements = true
			break
		}
	}
	require.True(t, sawImplements, "derived InferImplements pass must still run when ResolveAllContext returns ErrReindexWriteRefused")
}
