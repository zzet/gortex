package indexer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

var (
	errInjectedRepositoryPurge  = errors.New("injected repository purge failure")
	errInjectedVectorRefresh    = errors.New("injected vector refresh failure")
	errInjectedAllGenerationRun = errors.New("injected all-generation eviction failure")
	errInjectedConfigFinalize   = errors.New("injected config finalization failure")
)

type retryableRepositoryCleanupStore struct {
	graph.Store

	mu             sync.Mutex
	purgeFailures  int
	vectorFailures int
	purgeCalls     int
	vectorCalls    int
}

func (s *retryableRepositoryCleanupStore) PurgeRepo(prefix string) error {
	s.mu.Lock()
	s.purgeCalls++
	if s.purgeFailures > 0 {
		s.purgeFailures--
		s.mu.Unlock()
		return errInjectedRepositoryPurge
	}
	s.mu.Unlock()
	if purger, ok := s.Store.(interface{ PurgeRepo(string) error }); ok {
		return purger.PurgeRepo(prefix)
	}
	return nil
}

func (s *retryableRepositoryCleanupStore) ReplaceVectorCorpus(
	ctx context.Context, prefix string, dims int, items []graph.VectorCorpusItem,
) (graph.VectorCorpusStats, error) {
	s.mu.Lock()
	s.vectorCalls++
	if s.vectorFailures > 0 {
		s.vectorFailures--
		s.mu.Unlock()
		return graph.VectorCorpusStats{}, errInjectedVectorRefresh
	}
	s.mu.Unlock()
	if installer, ok := s.Store.(graph.AtomicVectorCorpusInstaller); ok {
		return installer.ReplaceVectorCorpus(ctx, prefix, dims, items)
	}
	return graph.VectorCorpusStats{Dims: dims}, nil
}

func (s *retryableRepositoryCleanupStore) VectorCorpusStats(
	ctx context.Context, dims int,
) (graph.VectorCorpusStats, error) {
	if installer, ok := s.Store.(graph.AtomicVectorCorpusInstaller); ok {
		return installer.VectorCorpusStats(ctx, dims)
	}
	return graph.VectorCorpusStats{}, nil
}

func (s *retryableRepositoryCleanupStore) VectorCorpusStatsForRepo(
	ctx context.Context, prefix string, dims int,
) (graph.VectorCorpusStats, error) {
	if installer, ok := s.Store.(graph.AtomicVectorCorpusInstaller); ok {
		return installer.VectorCorpusStatsForRepo(ctx, prefix, dims)
	}
	return graph.VectorCorpusStats{}, nil
}

func (s *retryableRepositoryCleanupStore) calls() (purge, vector int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeCalls, s.vectorCalls
}

type hiddenOptionalCleanupStore struct{ graph.Store }

type retryableAllGenerationStore struct {
	graph.Store

	mu        sync.Mutex
	failures  int
	calls     int
	retained  int
	edgesEach int
}

func (s *retryableAllGenerationStore) EvictRepoAllGenerationsChecked(string) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.failures > 0 {
		s.failures--
		return 0, 0, errInjectedAllGenerationRun
	}
	return s.retained, s.retained * s.edgesEach, nil
}

func (s *retryableAllGenerationStore) setFailures(failures int) {
	s.mu.Lock()
	s.failures = failures
	s.mu.Unlock()
}

func (s *retryableAllGenerationStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type repositoryCleanupPhases struct {
	indexerClosed   bool
	payloadPurged   bool
	vectorPublished bool
	configFinalized bool
}

func cleanupStateSnapshot(t testing.TB, mi *MultiIndexer, prefix string) (*repositoryUntrackState, repositoryCleanupPhases) {
	t.Helper()
	mi.mu.RLock()
	state := mi.pendingRepositoryUntracks[prefix]
	mi.mu.RUnlock()
	if state == nil {
		t.Fatalf("repository %q has no pending cleanup continuation", prefix)
	}
	state.mu.Lock()
	phases := repositoryCleanupPhases{
		indexerClosed:   state.indexerClosed,
		payloadPurged:   state.payloadPurged,
		vectorPublished: state.vectorPublished,
		configFinalized: state.configFinalized,
	}
	state.mu.Unlock()
	return state, phases
}

func assertSingleClosedCleanupLane(t testing.TB, mi *MultiIndexer, prefix string, state *repositoryUntrackState) {
	t.Helper()
	mi.repositoryMutationMu.Lock()
	defer mi.repositoryMutationMu.Unlock()
	if got := len(mi.repositoryMutations); got != 1 {
		t.Fatalf("mutation lane count = %d, want 1", got)
	}
	coordinator := mi.repositoryMutations[prefix]
	if coordinator == nil || coordinator != state.coordinator {
		t.Fatal("pending cleanup does not own the one stable mutation lane")
	}
	coordinator.mu.Lock()
	closed := coordinator.closed
	coordinator.mu.Unlock()
	if !closed {
		t.Fatal("pending cleanup mutation lane reopened before completion")
	}
}

func assertCleanupReleased(t testing.TB, mi *MultiIndexer, prefix string) {
	t.Helper()
	mi.mu.RLock()
	pending := len(mi.pendingRepositoryUntracks)
	mi.mu.RUnlock()
	if pending != 0 {
		t.Fatalf("pending cleanup count = %d, want 0", pending)
	}
	mi.repositoryMutationMu.Lock()
	lanes := len(mi.repositoryMutations)
	_, retained := mi.repositoryMutations[prefix]
	mi.repositoryMutationMu.Unlock()
	if lanes != 0 || retained {
		t.Fatalf("mutation lanes remain after cleanup: count=%d retained=%v", lanes, retained)
	}
}

func TestRepositoryCleanupRetriesEachFailedPhaseWithoutRepeatingCommittedWork(t *testing.T) {
	const prefix = "retryable"
	store := &retryableRepositoryCleanupStore{
		Store:          graph.New(),
		purgeFailures:  1,
		vectorFailures: 1,
	}
	mi := NewMultiIndexer(store, nil, nil, nil, zap.NewNop())
	metadata := &RepoMetadata{RepoPrefix: prefix, RootPath: t.TempDir(), NodeCount: 11, EdgeCount: 7}
	mi.mu.Lock()
	mi.repos[prefix] = metadata
	mi.indexers[prefix] = nil
	mi.mu.Unlock()

	finalizeCalls := 0
	finalize := func(got *RepoMetadata) error {
		finalizeCalls++
		if got != metadata {
			t.Fatalf("finalizer metadata = %p, want %p", got, metadata)
		}
		if finalizeCalls == 1 {
			return errInjectedConfigFinalize
		}
		return nil
	}

	if _, _, err := mi.purgeRepoChecked(context.Background(), prefix, finalize); !errors.Is(err, errInjectedRepositoryPurge) {
		t.Fatalf("first cleanup error = %v, want purge failure", err)
	}
	if mi.GetMetadata(prefix) != nil {
		t.Fatal("failed cleanup left repository visible")
	}
	state, phases := cleanupStateSnapshot(t, mi, prefix)
	if !phases.indexerClosed || phases.payloadPurged || phases.vectorPublished || phases.configFinalized {
		t.Fatalf("phases after purge failure = %+v", phases)
	}
	assertSingleClosedCleanupLane(t, mi, prefix, state)

	if _, _, err := mi.purgeRepoChecked(context.Background(), prefix, nil); !errors.Is(err, errInjectedVectorRefresh) {
		t.Fatalf("second cleanup error = %v, want vector failure", err)
	}
	_, phases = cleanupStateSnapshot(t, mi, prefix)
	if !phases.payloadPurged || phases.vectorPublished || phases.configFinalized {
		t.Fatalf("phases after vector failure = %+v", phases)
	}
	if purge, vector := store.calls(); purge != 2 || vector != 1 {
		t.Fatalf("calls after vector failure = purge:%d vector:%d, want 2/1", purge, vector)
	}
	if finalizeCalls != 0 {
		t.Fatalf("finalizer ran before payload/vector commit: %d", finalizeCalls)
	}

	if nodes, edges, err := mi.purgeRepoChecked(context.Background(), prefix, nil); !errors.Is(err, errInjectedConfigFinalize) {
		t.Fatalf("third cleanup = nodes:%d edges:%d err:%v, want config failure", nodes, edges, err)
	} else if nodes != 11 || edges != 7 {
		t.Fatalf("retained removal counts = %d/%d, want 11/7", nodes, edges)
	}
	_, phases = cleanupStateSnapshot(t, mi, prefix)
	if !phases.payloadPurged || !phases.vectorPublished || phases.configFinalized {
		t.Fatalf("phases after config failure = %+v", phases)
	}
	if purge, vector := store.calls(); purge != 2 || vector != 2 {
		t.Fatalf("completed destructive phases repeated: purge:%d vector:%d", purge, vector)
	}
	assertSingleClosedCleanupLane(t, mi, prefix, state)

	nodes, edges, err := mi.purgeRepoChecked(context.Background(), prefix, nil)
	if err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if nodes != 11 || edges != 7 {
		t.Fatalf("successful retry counts = %d/%d, want 11/7", nodes, edges)
	}
	if purge, vector := store.calls(); purge != 2 || vector != 2 {
		t.Fatalf("successful retry repeated destructive work: purge:%d vector:%d", purge, vector)
	}
	if finalizeCalls != 2 {
		t.Fatalf("finalizer calls = %d, want failed attempt plus retry", finalizeCalls)
	}
	assertCleanupReleased(t, mi, prefix)
}

func TestRepositoryCleanupFailsClosedWithoutAllGenerationCapabilityAndRetriesWithoutLiveRegistry(t *testing.T) {
	const prefix = "restart-equivalent"
	base := graph.New()
	mi := NewMultiIndexer(&hiddenOptionalCleanupStore{Store: base}, nil, nil, nil, zap.NewNop())

	if _, _, err := mi.purgeRepoChecked(context.Background(), prefix, nil); err == nil ||
		!strings.Contains(err.Error(), "does not expose all-generation repository eviction") {
		t.Fatalf("unsupported cleanup error = %v", err)
	}
	state, phases := cleanupStateSnapshot(t, mi, prefix)
	if phases.payloadPurged || phases.vectorPublished || phases.configFinalized {
		t.Fatalf("unsupported store advanced cleanup: %+v", phases)
	}
	assertSingleClosedCleanupLane(t, mi, prefix, state)

	checked := &retryableAllGenerationStore{Store: base, failures: 1, retained: 100, edgesEach: 2}
	mi.graph = checked
	if _, _, err := mi.purgeRepoChecked(context.Background(), prefix, nil); !errors.Is(err, errInjectedAllGenerationRun) {
		t.Fatalf("checked all-generation failure = %v", err)
	}
	if checked.callCount() != 1 {
		t.Fatalf("checked all-generation calls = %d, want 1", checked.callCount())
	}
	assertSingleClosedCleanupLane(t, mi, prefix, state)

	nodes, edges, err := mi.purgeRepoChecked(context.Background(), prefix, nil)
	if err != nil {
		t.Fatalf("restart-equivalent cleanup retry: %v", err)
	}
	if nodes != 100 || edges != 200 {
		t.Fatalf("all-generation removal counts = %d/%d, want 100/200", nodes, edges)
	}
	if checked.callCount() != 2 {
		t.Fatalf("checked all-generation calls after retry = %d, want 2", checked.callCount())
	}
	assertCleanupReleased(t, mi, prefix)
}

func TestRepositoryCleanupSagaRetriesConfigWriteWithoutRepeatingPayloadPurge(t *testing.T) {
	f := newFamilyFixture(t, "cleanup-config-retry")
	defer f.close()
	ctx := context.Background()

	configPath := f.cm.Global().ConfigPath()
	f.cm.Global().SetConfigPath(t.TempDir()) // an existing directory cannot be atomically replaced by a file
	counting := &retryableRepositoryCleanupStore{Store: f.store}
	f.mi.graph = counting

	_, err := f.lc.Untrack(ctx, f.main)
	if err == nil || !strings.Contains(err.Error(), "writing global config") {
		t.Fatalf("untrack config error = %v", err)
	}
	if !f.configLists(f.main) {
		t.Fatal("failed config write removed in-memory control-plane intent")
	}
	state, phases := cleanupStateSnapshot(t, f.mi, f.mainPrefix)
	if !phases.payloadPurged || !phases.vectorPublished || phases.configFinalized {
		t.Fatalf("cleanup phases after config write failure = %+v", phases)
	}
	assertSingleClosedCleanupLane(t, f.mi, f.mainPrefix, state)
	if purge, vector := counting.calls(); purge != 1 || vector != 1 {
		t.Fatalf("destructive phase calls after config failure = purge:%d vector:%d, want 1/1", purge, vector)
	}
	_, graphPresent, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	if err != nil {
		t.Fatalf("read graph after config failure: %v", err)
	}
	if !graphPresent {
		t.Fatal("cleanup saga deleted graph row after config finalization failed")
	}
	entries, err := f.catalog.ListCleanupEntries(ctx)
	if err != nil {
		t.Fatalf("list cleanup journal: %v", err)
	}
	failedCleanupID := ""
	for _, entry := range entries {
		if entry.Phase == store_sqlite.CleanupPhaseFailed &&
			strings.Contains(entry.OpaqueTargetIDs, `"phase":"release_graph"`) {
			failedCleanupID = entry.CleanupID
			break
		}
	}
	if failedCleanupID == "" {
		t.Fatalf("cleanup journal advanced past failed release_graph phase: %+v", entries)
	}

	f.cm.Global().SetConfigPath(configPath)
	if err := f.lc.rec.Resume(ctx); err != nil {
		t.Fatalf("resume cleanup after config path recovery: %v", err)
	}
	if f.configLists(f.main) {
		t.Fatal("successful config retry retained removed repository intent")
	}
	if purge, vector := counting.calls(); purge != 1 || vector != 1 {
		t.Fatalf("retry repeated committed destructive phases: purge:%d vector:%d", purge, vector)
	}
	_, graphPresent, err = f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	if err != nil {
		t.Fatalf("read graph after config retry: %v", err)
	}
	if graphPresent {
		t.Fatal("successful config retry retained dedicated graph row")
	}
	if _, present, err := f.catalog.GetCleanupEntry(ctx, failedCleanupID); err != nil {
		t.Fatalf("read completed config cleanup journal: %v", err)
	} else if present {
		t.Fatal("successful config retry retained failed cleanup entry")
	}
	assertCleanupReleased(t, f.mi, f.mainPrefix)
}

func TestRepositoryCleanupSagaRetainsGraphAndConfigThenResumesWithoutLiveRegistry(t *testing.T) {
	f := newFamilyFixture(t, "cleanup-saga-retry")
	defer f.close()
	ctx := context.Background()

	dedicated, graphPresent, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	if err != nil {
		t.Fatalf("read primary graph: %v", err)
	}
	if !graphPresent {
		t.Fatal("fixture did not bind the primary graph")
	}
	baseGeneration := dedicated.ActiveGenerationID
	baseHandle := f.store.AtGeneration(baseGeneration)
	if baseHandle == nil {
		t.Fatalf("fixture primary generation %d is invalid", baseGeneration)
	}
	beforePayload := baseHandle.RepoStats()[f.mainPrefix]
	if beforePayload.TotalNodes == 0 {
		t.Fatal("fixture did not build primary repository payload")
	}
	sidecarNodeID := f.mainPrefix + ":retry-vector-owner"
	f.store.AddNode(&graph.Node{
		ID: sidecarNodeID, Kind: graph.KindFunction, Name: "retryVectorOwner", RepoPrefix: f.mainPrefix,
	})
	if f.store.GetNode(sidecarNodeID) == nil {
		t.Fatal("fixture did not persist vector owner node")
	}
	if _, err := f.store.ReplaceVectorCorpus(ctx, f.mainPrefix, 2, []graph.VectorCorpusItem{{
		NodeID: sidecarNodeID,
		Vec:    []float32{0.25, 0.75},
	}}); err != nil {
		t.Fatalf("seed repository vector sidecar: %v", err)
	}
	beforeVectors, err := f.store.VectorCorpusStatsForRepo(ctx, f.mainPrefix, 2)
	if err != nil {
		t.Fatalf("read seeded vector sidecar: %v", err)
	}
	if beforeVectors.RepositoryVectorCount != 1 {
		t.Fatalf("seeded repository vectors = %d, want 1", beforeVectors.RepositoryVectorCount)
	}
	if !f.configLists(f.main) {
		t.Fatal("fixture config does not retain the explicit primary")
	}

	failing := &retryableRepositoryCleanupStore{Store: f.store, purgeFailures: 1}
	f.mi.graph = failing
	_, err = f.lc.Untrack(ctx, f.main)
	if !errors.Is(err, errInjectedRepositoryPurge) {
		t.Fatalf("untrack error = %v, want injected purge failure", err)
	}
	if f.mi.GetMetadata(f.mainPrefix) != nil {
		t.Fatal("failed authoritative cleanup left primary visible")
	}
	if !f.configLists(f.main) {
		t.Fatal("failed authoritative cleanup removed durable config intent")
	}
	if got := baseHandle.RepoStats()[f.mainPrefix].TotalNodes; got != beforePayload.TotalNodes {
		t.Fatalf("failed purge changed repository payload from %d to %d nodes", beforePayload.TotalNodes, got)
	}
	if _, present, err := f.catalog.GetViewGeneration(ctx, baseGeneration); err != nil {
		t.Fatalf("read primary generation after failed purge: %v", err)
	} else if !present {
		t.Fatal("failed purge deleted the primary payload generation")
	}
	if f.store.GetNode(sidecarNodeID) == nil {
		t.Fatal("failed purge deleted base payload before committing")
	}
	afterFailureVectors, err := f.store.VectorCorpusStatsForRepo(ctx, f.mainPrefix, 2)
	if err != nil {
		t.Fatalf("read vectors after failed purge: %v", err)
	}
	if afterFailureVectors.RepositoryVectorCount != 1 {
		t.Fatalf("failed purge changed vector sidecar count to %d", afterFailureVectors.RepositoryVectorCount)
	}
	_, graphPresent, err = f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	if err != nil {
		t.Fatalf("read graph after failed purge: %v", err)
	}
	if !graphPresent {
		t.Fatal("cleanup saga deleted graph row after ReleaseGraph failed")
	}

	entries, err := f.catalog.ListCleanupEntries(ctx)
	if err != nil {
		t.Fatalf("list cleanup journal: %v", err)
	}
	failedCleanupID := ""
	for _, entry := range entries {
		if entry.Phase == store_sqlite.CleanupPhaseFailed &&
			strings.Contains(entry.OpaqueTargetIDs, `"phase":"release_graph"`) {
			failedCleanupID = entry.CleanupID
			break
		}
	}
	if failedCleanupID == "" {
		t.Fatalf("cleanup journal did not retain failed release_graph phase: %+v", entries)
	}
	state, _ := cleanupStateSnapshot(t, f.mi, f.mainPrefix)
	assertSingleClosedCleanupLane(t, f.mi, f.mainPrefix, state)

	// Rebuild only the process-local indexer shell, as a daemon restart would.
	// The catalog journal and retained config are the sole authorities now; no
	// live repository registry or pending teardown continuation is carried over.
	restarted := NewMultiIndexer(f.store, f.mi.registry, f.mi.search, f.cm, zap.NewNop())
	f.mi = restarted
	f.lc.mi = restarted
	if err := f.lc.rec.Resume(ctx); err != nil {
		t.Fatalf("resume cleanup saga without live registry: %v", err)
	}

	if f.configLists(f.main) {
		t.Fatal("successful retry retained removed repository in config")
	}
	if got := baseHandle.RepoStats()[f.mainPrefix].TotalNodes; got != 0 {
		t.Fatalf("repository generation payload remains after retry: %d nodes", got)
	}
	if f.store.GetNode(sidecarNodeID) != nil {
		t.Fatal("base repository payload remains after retry")
	}
	afterRetryVectors, err := f.store.VectorCorpusStatsForRepo(ctx, f.mainPrefix, 2)
	if err != nil {
		t.Fatalf("read vectors after retry: %v", err)
	}
	if afterRetryVectors.RepositoryVectorCount != 0 || afterRetryVectors.RepositoryChunkCount != 0 {
		t.Fatalf("repository vector sidecars remain after retry: %+v", afterRetryVectors)
	}
	_, graphPresent, err = f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	if err != nil {
		t.Fatalf("read graph after retry: %v", err)
	}
	if graphPresent {
		t.Fatal("successful retry retained dedicated graph row")
	}
	if _, present, err := f.catalog.GetCleanupEntry(ctx, failedCleanupID); err != nil {
		t.Fatalf("read completed cleanup journal: %v", err)
	} else if present {
		t.Fatal("successful retry retained failed cleanup entry")
	}
	assertCleanupReleased(t, restarted, f.mainPrefix)
}

func BenchmarkRepositoryCleanupFailureRetry(b *testing.B) {
	for _, retained := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("retained_generations_%d", retained), func(b *testing.B) {
			store := &retryableAllGenerationStore{
				Store:     graph.New(),
				retained:  retained,
				edgesEach: 2,
			}
			mi := NewMultiIndexer(store, nil, nil, nil, zap.NewNop())
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				store.setFailures(1)
				if _, _, err := mi.purgeRepoChecked(ctx, "benchmark", nil); !errors.Is(err, errInjectedAllGenerationRun) {
					b.Fatalf("failure attempt: %v", err)
				}
				nodes, edges, err := mi.purgeRepoChecked(ctx, "benchmark", nil)
				if err != nil || nodes != retained || edges != retained*2 {
					b.Fatalf("retry = %d/%d, %v", nodes, edges, err)
				}
			}
		})
	}
}
