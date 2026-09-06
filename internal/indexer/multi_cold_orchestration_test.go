package indexer

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	store_sqlite "github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/semantic"
)

func coldOrchestrationRegistry() *parser.Registry {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	return reg
}

func coldOrchestrationRepos(t *testing.T, count int) []config.RepoEntry {
	t.Helper()
	base := t.TempDir()
	repos := make([]config.RepoEntry, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("cold-%02d", i)
		root := filepath.Join(base, name)
		require.NoError(t, os.MkdirAll(root, 0o755))
		source := fmt.Sprintf("package p\nfunc F%d() int { return %d }\n", i, i)
		require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte(source), 0o644))
		repos = append(repos, config.RepoEntry{Name: name, Path: root})
	}
	return repos
}

func newColdOrchestrationMulti(store graph.Store, logger *zap.Logger) *MultiIndexer {
	return NewMultiIndexer(store, coldOrchestrationRegistry(), search.NewNull(), nil, logger)
}

type coldOrchestrationProbeStore struct {
	*graph.Graph
	unresolvedScans atomic.Int64
	receiptBegins   atomic.Int64
	receiptEnds     atomic.Int64
}

func (s *coldOrchestrationProbeStore) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	s.unresolvedScans.Add(1)
	return s.Graph.EdgesWithUnresolvedTarget()
}

func (s *coldOrchestrationProbeStore) BeginMutationReceipt() graph.MutationReceiptToken {
	s.receiptBegins.Add(1)
	return s.Graph.BeginMutationReceipt()
}

func (s *coldOrchestrationProbeStore) EndMutationReceipt(token graph.MutationReceiptToken) graph.MutationReceipt {
	s.receiptEnds.Add(1)
	return s.Graph.EndMutationReceipt(token)
}

func TestBeginDeferredPassesOverlapOpensReceiptAtApplyBoundary(t *testing.T) {
	store := &coldOrchestrationProbeStore{Graph: graph.New()}
	mi := newColdOrchestrationMulti(store, zap.NewNop())
	applyGate := make(chan struct{})
	run := mi.BeginDeferredPasses(t.Context(), applyGate)

	// Model pre-enrichment resolver writes: no receipt exists yet, so they
	// cannot void the exact apply/contract window.
	store.AddNode(&graph.Node{ID: "pre-resolve", Kind: graph.KindFile, FilePath: "pre.go"})
	assert.Zero(t, store.receiptBegins.Load())

	run.BeginApplyMutationReceipt()
	run.BeginApplyMutationReceipt()
	assert.Equal(t, int64(1), store.receiptBegins.Load(), "apply receipt must open exactly once")
	close(applyGate)

	result := run.FinishTailResult()
	assert.True(t, result.ExactCrossRepoComplete)
	assert.Equal(t, int64(1), store.receiptEnds.Load(), "the late receipt must close in FinishTail")
}

func TestFinishColdDeferredPassesDoesNotRepeatFullCrossRepoResolve(t *testing.T) {
	store := &coldOrchestrationProbeStore{Graph: graph.New()}
	mi := newColdOrchestrationMulti(store, zap.NewNop())

	result := mi.finishColdDeferredPasses(t.Context(), nil)

	assert.True(t, result.ExactCrossRepoComplete)
	assert.Zero(t, store.unresolvedScans.Load(),
		"cold deferred completion must not repeat the full unresolved-edge scan already run before enrichment")
	assert.Equal(t, int64(1), store.receiptBegins.Load())
	assert.Equal(t, int64(1), store.receiptEnds.Load())
}

func TestIndexMultiRepoUsesOneCoordinatedBaseResolve(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	mi := newColdOrchestrationMulti(graph.New(), zap.New(core))
	repos := coldOrchestrationRepos(t, 8)

	results, err := mi.indexMultiRepo(repos)
	require.NoError(t, err)
	require.Len(t, results, len(repos))

	assert.Len(t, logs.FilterMessage("DEFERRED-TIMING master.ResolveAll").All(), 1,
		"cold multi-repo indexing must run one shared base resolver")
	assert.Empty(t, logs.FilterMessage("DEFERRED-TIMING per-repo").All(),
		"the coordinated path must not invoke per-repo RunDeferredPasses")
	assert.Len(t, logs.FilterMessage("multi-repo coordinated deferred passes complete").All(), 1)
	for prefix, idx := range mi.indexers {
		assert.False(t, idx.skipResolveInDeferred, "%s retained the batch-only resolve gate", prefix)
		assert.Nil(t, idx.pendingContractReg, "%s retained committed contract work", prefix)
		assert.False(t, idx.deferredGoModDone, "%s retained the completed generation guard", prefix)
	}
}

func TestIndexMultiRepoAllFailuresDoNotRunCompletionPipeline(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	mi := newColdOrchestrationMulti(graph.New(), zap.New(core))
	repos := make([]config.RepoEntry, 0, 4)
	for i := 0; i < 4; i++ {
		repos = append(repos, config.RepoEntry{
			Name: fmt.Sprintf("missing-%02d", i),
			Path: filepath.Join(t.TempDir(), "does-not-exist"),
		})
	}

	results, err := mi.indexMultiRepo(repos)
	require.Error(t, err)
	assert.Nil(t, results)
	assert.Empty(t, logs.FilterMessage("DEFERRED-TIMING master.ResolveAll").All())
	assert.Empty(t, logs.FilterMessage("multi-repo coordinated deferred passes complete").All())
	assert.Empty(t, logs.FilterMessage("global passes complete").All())
}

type coldBatchProvider struct {
	language string
	fail     bool
	delay    time.Duration
	calls    atomic.Int32
	inFlight atomic.Int32
	max      atomic.Int32
}

func (p *coldBatchProvider) Name() string        { return "cold-batch-" + p.language }
func (p *coldBatchProvider) Languages() []string { return []string{p.language} }
func (p *coldBatchProvider) Available() bool     { return true }
func (p *coldBatchProvider) Close() error        { return nil }

func (p *coldBatchProvider) Enrich(graph.Store, string) (*semantic.EnrichResult, error) {
	p.calls.Add(1)
	current := p.inFlight.Add(1)
	for {
		prior := p.max.Load()
		if current <= prior || p.max.CompareAndSwap(prior, current) {
			break
		}
	}
	defer p.inFlight.Add(-1)
	time.Sleep(p.delay)
	if p.fail {
		return nil, errors.New("injected enrichment failure")
	}
	return &semantic.EnrichResult{Provider: p.Name(), Language: p.language}, nil
}

func (p *coldBatchProvider) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	return p.Enrich(nil, "")
}

func deferredBatchFixture(t *testing.T, language string, count int, provider *coldBatchProvider) *MultiIndexer {
	t.Helper()
	// These fixtures carry one node per repo; disable the enrichment
	// admission floor so pool-mechanics tests exercise scheduling, not
	// admission (which has its own tests in internal/semantic).
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	store := graph.New()
	manager := semantic.NewManager(semantic.Config{
		Enabled: true,
		Providers: []semantic.ProviderConfig{{
			Name: provider.Name(), Languages: provider.Languages(), Priority: 1, Enabled: true,
		}},
	}, zap.NewNop())
	manager.RegisterProvider(provider)
	mi := newColdOrchestrationMulti(store, zap.NewNop())
	mi.SetSemanticManager(manager)
	root := t.TempDir()
	for i := 0; i < count; i++ {
		prefix := fmt.Sprintf("batch-%02d", i)
		idx := mi.newPerRepoIndexer(config.Default().Index)
		idx.SetRepoPrefix(prefix)
		idx.SetRootPath(root)
		idx.markPendingEnrichFull()
		mi.indexers[prefix] = idx
		store.AddNode(&graph.Node{
			ID: prefix + "/file", Kind: graph.KindFile, Name: "file",
			FilePath: prefix + "/file", Language: language, RepoPrefix: prefix,
		})
	}
	return mi
}

func TestRunDeferredPassesAllPoolsEnrichmentBounded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		language string
		goNodes  int
	}{
		{name: "ordinary providers flow through bounded pool lanes", language: "python"},
		// A repo with only trace Go (grammar bindings, helper tools) must NOT
		// serialize the schedule — that head-of-line pattern is what turned
		// 26-repo enrichment into a strictly serial 34-minute phase. Real
		// go/packages concurrency stays bounded by the provider's heavy gate.
		{name: "light Go repositories pool in parallel", language: "go", goNodes: 1},
		// Heavy Go repositories lead the queue but still occupy only one
		// lane; their exclusivity lives in the go/types heavy gate.
		{name: "heavy Go repositories lead the queue", language: "go", goNodes: goHeavyEnrichNodeThreshold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &coldBatchProvider{language: tc.language, delay: 30 * time.Millisecond}
			mi := deferredBatchFixture(t, tc.language, 8, provider)
			indexers := make([]*Indexer, 0, len(mi.indexers))
			langSets := make(map[*Indexer][]string, len(mi.indexers))
			goCounts := make(map[*Indexer]int, len(mi.indexers))
			for _, idx := range mi.indexers {
				indexers = append(indexers, idx)
				langSets[idx] = []string{tc.language}
				if tc.language == "go" {
					goCounts[idx] = tc.goNodes
				}
			}
			queue := deferredEnrichQueue(indexers, langSets, goCounts)
			if len(queue) != len(indexers) {
				t.Fatalf("queue dropped repos: %d of %d", len(queue), len(indexers))
			}

			scheduled := mi.RunDeferredPassesAllResult(t.Context()).EnrichScheduled
			require.Equal(t, 8, scheduled)
			assert.Equal(t, int32(8), provider.calls.Load())
			// The pool bounds concurrent per-repo passes at enrichConcurrency
			// regardless of language mix.
			assert.LessOrEqual(t, provider.max.Load(), int32(enrichConcurrency(8)))
		})
	}
}

func TestRunDeferredPassesAllFailureKeepsGenerationPending(t *testing.T) {
	provider := &coldBatchProvider{language: "go", fail: true}
	mi := deferredBatchFixture(t, "go", 1, provider)

	require.Equal(t, 1, mi.RunDeferredPassesAllResult(t.Context()).EnrichScheduled)
	idx := mi.indexers["batch-00"]
	require.NotNil(t, idx)
	assert.True(t, idx.pendingEnrich.Load(),
		"a failed provider must not publish completion for its pending generation")
}

func serialColdReference(t *testing.T, repos []config.RepoEntry) *MultiIndexer {
	t.Helper()
	mi := newColdOrchestrationMulti(graph.New(), zap.NewNop())
	prefixes := make([]string, 0, len(repos))
	for _, entry := range repos {
		absPath, err := filepath.Abs(entry.Path)
		require.NoError(t, err)
		prefix := config.ResolvePrefix(entry)
		idx := mi.newPerRepoIndexer(config.Default().Index)
		idx.SetRepoPrefix(prefix)
		idx.SetWorkspaceID(prefix)
		idx.SetProjectID(prefix)
		idx.SetDeferResolve(true)
		// Match the production cold path: this Indexer is not live until the
		// serial fixture publishes it below, so initialize it with the raw
		// primitive while the fixture owns orchestration.
		result, err := idx.indexCtxRaw(context.Background(), absPath)
		require.NoError(t, err)
		result.RepoPrefix = prefix
		mi.indexers[prefix] = idx
		mi.repos[prefix] = &RepoMetadata{RepoPrefix: prefix, RootPath: absPath}
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		mi.indexers[prefix].RunDeferredPasses(t.Context())
	}
	mi.runCrossRepoResolve(true)
	mi.runGlobalGraphPasses(t.Context(), nil, false)
	return mi
}

type coldGraphShape struct {
	nodes []string
	edges []string
}

func snapshotColdGraph(store graph.Store, repos []config.RepoEntry) coldGraphShape {
	prefixes := make([]string, 0, len(repos)+1)
	prefixes = append(prefixes, "")
	for _, entry := range repos {
		prefixes = append(prefixes, config.ResolvePrefix(entry))
	}
	shape := coldGraphShape{}
	for _, prefix := range prefixes {
		for _, node := range store.GetRepoNodes(prefix) {
			shape.nodes = append(shape.nodes, fmt.Sprintf("%s|%s|%s|%s", node.ID, node.Kind, node.Name, node.FilePath))
		}
		if prefix == "" {
			continue
		}
		for _, edge := range store.GetRepoEdges(prefix) {
			shape.edges = append(shape.edges, fmt.Sprintf("%s|%s|%s|%s", edge.From, edge.To, edge.Kind, edge.Origin))
		}
	}
	sort.Strings(shape.nodes)
	sort.Strings(shape.edges)
	return shape
}

func TestCoordinatedColdPipelineMatchesSerialReferenceGraph(t *testing.T) {
	repos := coldOrchestrationRepos(t, 5)
	coordinated := newColdOrchestrationMulti(graph.New(), zap.NewNop())
	_, err := coordinated.indexMultiRepo(repos)
	require.NoError(t, err)
	serial := serialColdReference(t, repos)

	assert.Equal(t, snapshotColdGraph(serial.graph, repos), snapshotColdGraph(coordinated.graph, repos))
}

type coldRefFactsCountingStore struct {
	*store_sqlite.Store
	rebuildCalls    int
	rebuildPrefixes []string
	rebuildErr      error
}

func (s *coldRefFactsCountingStore) RebuildRefFactsForRepos(repoPrefixes []string) error {
	s.rebuildCalls++
	s.rebuildPrefixes = append([]string(nil), repoPrefixes...)
	if s.rebuildErr != nil {
		return s.rebuildErr
	}
	return s.Store.RebuildRefFactsForRepos(repoPrefixes)
}

type coldRefFactsProbeStore struct {
	graph.Store
	rebuildCalls    int
	rebuildPrefixes []string
	rebuildErr      error
}

func (s *coldRefFactsProbeStore) RebuildRefFactsForRepos(repoPrefixes []string) error {
	s.rebuildCalls++
	s.rebuildPrefixes = append([]string(nil), repoPrefixes...)
	return s.rebuildErr
}

func (s *coldRefFactsProbeStore) ReplaceRefFactsForFiles(string, []string) error { return nil }

func coldRefFactRepos(t *testing.T, count int) []config.RepoEntry {
	t.Helper()
	base := t.TempDir()
	repos := make([]config.RepoEntry, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("cold-ref-%02d", i)
		root := filepath.Join(base, name)
		require.NoError(t, os.MkdirAll(root, 0o755))
		source := fmt.Sprintf("package p\nfunc Target%d() int { return %d }\nfunc Caller%d() int { return Target%d() }\n", i, i, i, i)
		require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte(source), 0o644))
		repos = append(repos, config.RepoEntry{Name: name, Path: root})
	}
	return repos
}

func refFactSemanticShape(facts []graph.RefFact) []string {
	shape := make([]string, 0, len(facts))
	for _, fact := range facts {
		shape = append(shape, fmt.Sprintf("%s|%s|%d|%s|%s|%s", fact.Kind, fact.RefName, fact.Line, fact.Origin, fact.Tier, fact.Lang))
	}
	sort.Strings(shape)
	return shape
}

func TestIndexMultiRepoRefFactsMatchesSingleFullAndUsesOneSetRebuild(t *testing.T) {
	repos := coldRefFactRepos(t, 3)
	requested := append([]config.RepoEntry(nil), repos...)
	requested = append(requested, config.RepoEntry{
		Name: "cold-ref-missing",
		Path: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "multi.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	counting := &coldRefFactsCountingStore{Store: store}
	multi := newColdOrchestrationMulti(counting, zap.NewNop())

	results, err := multi.indexMultiRepo(requested)
	require.NoError(t, err)
	require.Len(t, results, len(repos))
	require.Equal(t, 1, counting.rebuildCalls,
		"all repository prefixes must share one set-oriented backend rebuild")
	wantPrefixes := make([]string, 0, len(results))
	for prefix := range results {
		wantPrefixes = append(wantPrefixes, prefix)
	}
	sort.Strings(wantPrefixes)
	require.Equal(t, wantPrefixes, counting.rebuildPrefixes)

	for _, entry := range repos {
		prefix := config.ResolvePrefix(entry)
		multiFacts, loadErr := store.LoadRefFactsByFiles(prefix, nil)
		require.NoError(t, loadErr)
		require.NotEmpty(t, multiFacts, "%s cold multi index did not seed ref_facts", prefix)

		singleStore, openErr := store_sqlite.Open(filepath.Join(t.TempDir(), prefix+"-single.sqlite"))
		require.NoError(t, openErr)
		single := New(singleStore, coldOrchestrationRegistry(), config.Default().Index, zap.NewNop())
		_, indexErr := single.Index(entry.Path)
		require.NoError(t, indexErr)
		// ResolveAll is the canonical full-index persistence boundary this
		// coordinated path replaces. Invoke it explicitly so the reference does
		// not depend on Index's caller-controlled deferred-resolve setting.
		single.ResolveAll()
		singleFacts, loadErr := singleStore.LoadRefFactsByFiles("", nil)
		require.NoError(t, loadErr)
		require.NotEmpty(t, singleFacts)
		assert.Equal(t, refFactSemanticShape(singleFacts), refFactSemanticShape(multiFacts), prefix)
		require.NoError(t, singleStore.Close())
	}
}

func TestColdRefFactRebuildPreservesFailureAndCancellation(t *testing.T) {
	sentinel := errors.New("injected ref-fact rebuild failure")
	probe := &coldRefFactsProbeStore{Store: graph.New(), rebuildErr: sentinel}
	mi := newColdOrchestrationMulti(probe, zap.NewNop())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := mi.rebuildColdRefFacts(ctx, []string{"repo-a", "repo-b"})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, probe.rebuildCalls, "cancellation before the boundary must skip the rebuild")

	err = mi.rebuildColdRefFacts(t.Context(), []string{"repo-a", "repo-b"})
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, 1, probe.rebuildCalls)
	require.Equal(t, []string{"repo-a", "repo-b"}, probe.rebuildPrefixes)
}

func TestIndexMultiRepoRefFactFailureStopsGlobalConsumers(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	sentinel := errors.New("injected ref-fact rebuild failure")
	probe := &coldRefFactsProbeStore{Store: graph.New(), rebuildErr: sentinel}
	mi := newColdOrchestrationMulti(probe, zap.New(core))

	results, err := mi.indexMultiRepo(coldRefFactRepos(t, 2))
	require.ErrorIs(t, err, sentinel)
	require.Len(t, results, 2, "successful parse results remain available with the completion error")
	require.Equal(t, 1, probe.rebuildCalls)
	assert.Empty(t, logs.FilterMessage("global passes complete").All(),
		"a failed reference-fact boundary must not publish global completion")
}

// TestBeginDeferredPassesOverlapSplitsPoolFromTail pins the overlap contract:
// BeginDeferredPasses drains the enrichment pool on its own goroutine while
// the caller is free to run the resolve phase; contracts and the catch-up
// resolve happen only in FinishTailResult, and the batch-only resolve gate is
// restored afterwards.
func TestBeginDeferredPassesOverlapSplitsPoolFromTail(t *testing.T) {
	provider := &coldBatchProvider{language: "python", delay: 15 * time.Millisecond}
	mi := deferredBatchFixture(t, "python", 4, provider)

	run := mi.BeginDeferredPasses(t.Context(), nil)
	run.Wait()
	assert.Equal(t, int32(4), provider.calls.Load(),
		"the pool must drain without FinishTailResult being called")
	for prefix, idx := range mi.indexers {
		assert.True(t, idx.skipResolveInDeferred,
			"%s must keep the batch-only resolve gate until the tail runs", prefix)
	}

	scheduled := run.FinishTailResult().EnrichScheduled
	assert.Equal(t, 4, scheduled)
	for prefix, idx := range mi.indexers {
		assert.False(t, idx.skipResolveInDeferred,
			"%s retained the batch-only resolve gate after the tail", prefix)
		assert.False(t, idx.pendingEnrich.Load(),
			"%s did not publish enrichment completion", prefix)
	}
}
