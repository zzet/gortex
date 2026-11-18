package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Retain the real SQLite surface and intercept only the first completed
// structural AddBatch. The ordinary post-write file-version validation, retry,
// deferred resolver and watcher receipt consumer all remain production code.
type versionRetryRelationStore struct {
	*store_sqlite.Store
	armed   atomic.Bool
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *versionRetryRelationStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.Store.AddBatch(nodes, edges)
	if s.armed.Load() {
		s.once.Do(func() {
			close(s.entered)
			<-s.release
		})
	}
}

func TestVersionRetryDoesNotTreatUnresolvedNewCallAsPriorAttempt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	base := "package main\nfunc Target() {}\n"
	first := base + "func Earlier() { Target() }\n"
	last := first + "func Later() { Target() }\n"
	require.NoError(t, os.WriteFile(path, []byte(base), 0600))
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "version-retry.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := &versionRetryRelationStore{Store: db, entered: make(chan struct{}), release: make(chan struct{})}
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := New(store, registry, config.Default().Index, logger)
	idx.resolver.SetLogger(logger)
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.NoError(t, idx.IndexFile(path))
	watcher, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, logger)
	require.NoError(t, err)
	// This component isolates the exact observed version-race retry from
	// independent native echoes, which remain covered by the cadence case.
	require.NoError(t, watcher.Start([]string{t.TempDir()}))
	unblock := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(func() {
		unblock()
		require.NoError(t, watcher.Stop())
		for _, entry := range logs.All() {
			if entry.Message == "incremental reindex: failed to index file" || entry.Message == "resolver: incremental files phases" {
				t.Logf("actual version-retry pipeline %s: %v", entry.Message, entry.ContextMap())
			}
		}
	})
	require.NoError(t, os.WriteFile(path, []byte(first), 0600))
	store.armed.Store(true)
	one, err := watcher.EnqueueToolMutation(t.Context(), path)
	require.NoError(t, err)
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first structural AddBatch was not observed")
	}
	prior := db.GetOutEdges("main.go::Earlier")
	require.Len(t, prior, 1)
	require.Equal(t, graph.UnresolvedMarker+"Target", prior[0].To, "first parse has not run its deferred resolver")
	// Change source only after the first graph write. No fabricated failure:
	// the actual version fence must discover it and exercise its own retry.
	require.NoError(t, os.WriteFile(path, []byte(last), 0600))
	two, err := watcher.EnqueueToolMutation(t.Context(), path)
	require.NoError(t, err)
	require.Greater(t, two.Generation, one.Generation)
	unblock()
	for _, ticket := range []*MutationTicket{one, two} {
		result := awaitMutationRetryResult(t, ticket)
		t.Logf("actual completed ticket=%d result=%+v tiers=%+v", ticket.Generation, result, ticket.TierState())
		require.NoError(t, result.Err)
		require.True(t, result.Reindexed)
		require.GreaterOrEqual(t, result.AppliedGeneration, ticket.Generation)
		require.False(t, ticket.TierState().RelationsAt.IsZero())
	}
	observedRace := false
	for _, entry := range logs.All() {
		if entry.Message == "incremental reindex: failed to index file" && entry.ContextMap()["error"] == errFileVersionChanged.Error() {
			observedRace = true
		}
	}
	require.True(t, observedRace, "actual post-apply version-race retry is the required path")
	require.Zero(t, watcher.TierState(path).Generation, "both own tickets must be retired, with no later pending generation")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, last, string(raw))
	before := db.MutationRevision()
	earlier := db.GetOutEdges("main.go::Earlier")
	later := db.GetOutEdges("main.go::Later")
	after := db.MutationRevision()
	t.Logf("settled own edges earlier=%+v later=%+v revision=%d/%d", earlier, later, before, after)
	require.Equal(t, before, after, "no subsequent graph mutation may explain these final reads")
	require.Len(t, later, 1)
	require.Equal(t, "main.go::Target", later[0].To, "retry must resolve its newly added call")
	require.Len(t, earlier, 1)
	require.Equal(t, "main.go::Target", earlier[0].To, "the first parse's never-attempted call cannot be skipped as a historical unresolved verdict")
}

// First-file evidence is independent across a multi-file batch. Preserve real
// historical pending rows and first declarations, but never add a retry's
// never-resolved rows to the skip set.
func TestDeferredResolverRetryEvidenceKeepsFirstFileSurfaces(t *testing.T) {
	firstA := resolver.DeclarationSurfaceOf([]*graph.Node{{ID: "a.go::First", Name: "First", Kind: graph.KindFunction, FilePath: "a.go"}})
	retryA := resolver.DeclarationSurfaceOf([]*graph.Node{{ID: "a.go::Retry", Name: "Retry", Kind: graph.KindFunction, FilePath: "a.go"}})
	firstB := resolver.DeclarationSurfaceOf([]*graph.Node{{ID: "b.go::First", Name: "First", Kind: graph.KindFunction, FilePath: "b.go"}})
	oldA := &graph.Edge{From: "a.go::First", To: graph.UnresolvedMarker + "External", Kind: graph.EdgeCalls, FilePath: "a.go"}
	newA := &graph.Edge{From: "a.go::Retry", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: "a.go"}
	oldB := &graph.Edge{From: "b.go::First", To: graph.UnresolvedMarker + "OtherExternal", Kind: graph.EdgeCalls, FilePath: "b.go"}
	batch := &reparsePendingEnrichmentBatch{}
	batch.recordDeferredResolverEvidence(true, []string{"a.go"}, map[string]resolver.DeclarationSurface{"a.go": firstA}, []*graph.Edge{oldA})
	batch.recordDeferredResolverEvidence(true, []string{"a.go", "b.go"}, map[string]resolver.DeclarationSurface{"a.go": retryA, "b.go": firstB}, []*graph.Edge{newA, oldB})
	declarations, pending := batch.takeDeferredResolverEvidence()
	require.Equal(t, map[string]resolver.DeclarationSurface{"a.go": firstA, "b.go": firstB}, declarations)
	require.Equal(t, []*graph.Edge{oldA, oldB}, pending, "original pending evidence stays scoped independently per file")
	declarations, pending = batch.takeDeferredResolverEvidence()
	require.Nil(t, declarations, "evidence is consumed exactly once")
	require.Nil(t, pending)
	batch.recordDeferredResolverEvidence(true, []string{"a.go"}, map[string]resolver.DeclarationSurface{"a.go": retryA}, []*graph.Edge{newA})
	declarations, pending = batch.takeDeferredResolverEvidence()
	require.Equal(t, map[string]resolver.DeclarationSurface{"a.go": retryA}, declarations, "next catch-up owns a new first surface")
	require.Equal(t, []*graph.Edge{newA}, pending, "consumption clears file observation as well as payload")
}

func TestDeferredResolverRetryEvidenceDisabledRecordsNothing(t *testing.T) {
	batch := &reparsePendingEnrichmentBatch{}
	batch.recordDeferredResolverEvidence(false, []string{"a.go"}, map[string]resolver.DeclarationSurface{"a.go": {}}, []*graph.Edge{{From: "a.go::Call", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: "a.go"}})
	declarations, pending := batch.takeDeferredResolverEvidence()
	require.Nil(t, declarations)
	require.Nil(t, pending)
}

func TestDeferredResolverRetryEvidenceFirstEmptyStillWins(t *testing.T) {
	batch := &reparsePendingEnrichmentBatch{}
	batch.recordDeferredResolverEvidence(true, []string{"empty.go"}, map[string]resolver.DeclarationSurface{"empty.go": {}}, nil)
	batch.recordDeferredResolverEvidence(true, []string{"empty.go"}, map[string]resolver.DeclarationSurface{"empty.go": {}}, []*graph.Edge{{From: "empty.go::New", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: "empty.go"}})
	_, pending := batch.takeDeferredResolverEvidence()
	require.Nil(t, pending, "empty original evidence must not admit intermediate unresolved rows")
}

func TestDeferredResolverRetryEvidenceUsingChangeKeepsExhaustiveFallback(t *testing.T) {
	file := "using.cs"
	oldNodes := []*graph.Node{{ID: file, Kind: graph.KindFile, FilePath: file, Meta: map[string]any{"usings": []string{"Old"}}}}
	newNodes := []*graph.Node{{ID: file, Kind: graph.KindFile, FilePath: file, Meta: map[string]any{"usings": []string{"New"}}}, {ID: file + "::Call", Kind: graph.KindMethod, Name: "Call", FilePath: file}}
	first := stagePriorDeclarations([]*incrementalBatchStage{{graphPath: file, priorNodes: oldNodes, result: &parser.ExtractionResult{Nodes: newNodes}}})
	require.Empty(t, first, "actual using-stamp change deliberately omits declaration scoping")
	retry := stagePriorDeclarations([]*incrementalBatchStage{{graphPath: file, priorNodes: newNodes, result: &parser.ExtractionResult{Nodes: newNodes}}})
	require.NotEmpty(t, retry, "retry alone would now appear declaration-eligible")
	batch := &reparsePendingEnrichmentBatch{}
	batch.recordDeferredResolverEvidence(true, []string{file}, first, nil)
	batch.recordDeferredResolverEvidence(true, []string{file}, retry, []*graph.Edge{{From: file + "::Call", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: file}})
	declarations, pending := batch.takeDeferredResolverEvidence()
	require.Nil(t, declarations, "first omitted scope must keep the conservative incoming fallback")
	require.Nil(t, pending, "retry must not skip newly parsed calls")
}
