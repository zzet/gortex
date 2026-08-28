package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

const (
	trackGuardFamily   = "family-track-guard"
	trackGuardCheckout = "checkout-track-guard"
	trackGuardPrefix   = "repo"
)

type trackGuardFixture struct {
	srv           *Server
	store         *store_sqlite.Store
	catalog       *store_sqlite.Catalog
	configManager *config.ConfigManager
	multiIndexer  *indexer.MultiIndexer
	configPath    string
	repoRoot      string
	graphID       string
}

func newTrackGuardFixture(tb testing.TB, readiness string) *trackGuardFixture {
	tb.Helper()
	base := tb.TempDir()
	repoRoot := filepath.Join(base, trackGuardPrefix)
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		tb.Fatalf("mkdir repository: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "repo.go"), []byte("package repo\n\nfunc Ready() {}\n"), 0o644); err != nil {
		tb.Fatalf("write repository: %v", err)
	}

	configPath := filepath.Join(base, "config.yaml")
	global := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoRoot, Name: trackGuardPrefix}}}
	global.SetConfigPath(configPath)
	if err := global.Save(); err != nil {
		tb.Fatalf("save config: %v", err)
	}
	configManager, err := config.NewConfigManager(configPath)
	if err != nil {
		tb.Fatalf("config manager: %v", err)
	}

	store, err := store_sqlite.Open(filepath.Join(base, "store.sqlite"))
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { _ = store.Close() })

	registry := testRegistry()
	multiIndexer := indexer.NewMultiIndexer(store, registry, search.NewNull(), configManager, zap.NewNop())
	tb.Cleanup(func() { _ = multiIndexer.Close(context.Background()) })
	if _, err := multiIndexer.IndexScoped("", ""); err != nil {
		tb.Fatalf("index repository: %v", err)
	}
	if multiIndexer.GetIndexer(trackGuardPrefix) == nil {
		tb.Fatal("fixture did not install the process-local repository indexer")
	}

	ctx := context.Background()
	catalog := store.Catalog()
	graphID := indexer.GraphIDFor(trackGuardPrefix)
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          trackGuardFamily,
		CommonDirIdentity: filepath.Join(repoRoot, ".git"),
		State:             reconcile.FamilyStateReady,
		CreatedAt:         1,
		LastSeen:          1,
	}); err != nil {
		tb.Fatalf("seed family: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    trackGuardCheckout,
		Incarnation:   "inc-track-guard",
		FamilyID:      trackGuardFamily,
		RootPath:      repoRoot,
		GitDir:        filepath.Join(repoRoot, ".git"),
		AdminName:     "primary",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      1,
	}); err != nil {
		tb.Fatalf("seed checkout: %v", err)
	}
	if err := catalog.UpsertTrackingIntent(ctx, store_sqlite.TrackingIntent{
		IntentID:      "intent-track-guard",
		CheckoutID:    trackGuardCheckout,
		SourceKind:    store_sqlite.IntentSourceMCPTrack,
		SourceLocator: "track-guard-test",
		Active:        true,
		CreatedAt:     1,
	}); err != nil {
		tb.Fatalf("seed tracking intent: %v", err)
	}
	graph := store_sqlite.DedicatedGraph{
		GraphID:         graphID,
		OwnerCheckoutID: trackGuardCheckout,
		RepoPrefix:      trackGuardPrefix,
		FamilyID:        trackGuardFamily,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	}
	if err := catalog.UpsertDedicatedGraph(ctx, graph); err != nil {
		tb.Fatalf("seed dedicated graph: %v", err)
	}
	generationID, _, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        graphID,
		LayerID:        "layer-track-guard",
		CheckoutID:     trackGuardCheckout,
		GenerationKind: "commit",
		TreeOID:        "tree-track-guard",
		CreatedAt:      1,
	})
	if err != nil {
		tb.Fatalf("begin generation: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 2); err != nil {
		tb.Fatalf("publish generation: %v", err)
	}
	graph.ActiveGenerationID = generationID
	if err := catalog.UpsertDedicatedGraph(ctx, graph); err != nil {
		tb.Fatalf("activate dedicated generation: %v", err)
	}
	if err := catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         trackGuardCheckout,
		GraphID:            graphID,
		CommitGenerationID: generationID,
		State:              store_sqlite.RouteActive,
	}); err != nil {
		tb.Fatalf("seed checkout route: %v", err)
	}

	engine := query.NewEngine(store)
	srv := NewServer(engine, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: configManager,
		MultiIndexer:  multiIndexer,
	})
	srv.SetMaterializer(&graphview.Materializer{
		Store:   store,
		Catalog: catalog,
		Leases:  graphview.NewLeaseManager(),
	})
	tb.Cleanup(func() {
		if srv.lifecycle != nil {
			_ = srv.lifecycle.Close()
		}
	})
	switch readiness {
	case "ready":
		srv.PublishReadiness("ready", true, nil)
	case "warming":
		srv.PublishReadiness("parallel_parse", false, nil)
	case "unknown":
	default:
		tb.Fatalf("unknown readiness fixture %q", readiness)
	}
	return &trackGuardFixture{
		srv:           srv,
		store:         store,
		catalog:       catalog,
		configManager: configManager,
		multiIndexer:  multiIndexer,
		configPath:    configPath,
		repoRoot:      repoRoot,
		graphID:       graphID,
	}
}

func trackGuardRequest(path string) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": path}
	return req
}

func assertTrackInFlightEmpty(t *testing.T, srv *Server, path string) {
	t.Helper()
	_, found := srv.trackInFlight.Load(filepath.Clean(path))
	assert.False(t, found, "track admission leaked for %s", path)
}

func TestTrackRepositoryUnavailableHasNoSideEffects(t *testing.T) {
	for _, readiness := range []string{"unknown", "warming"} {
		t.Run(readiness, func(t *testing.T) {
			fixture := newTrackGuardFixture(t, readiness)
			before, err := os.ReadFile(fixture.configPath)
			require.NoError(t, err)
			var registers atomic.Int32
			var launches atomic.Int32

			res, err := fixture.srv.handleTrackRepositoryWithRuntime(
				context.Background(),
				trackGuardRequest(fixture.repoRoot),
				trackRepositoryRuntime{
					register: func(context.Context, config.RepoEntry, store_sqlite.IntentSourceKind) (indexer.RegisterResult, error) {
						registers.Add(1)
						return indexer.RegisterResult{}, nil
					},
					launch: func(run func()) {
						launches.Add(1)
						run()
					},
				},
			)
			require.NoError(t, err)
			require.NotNil(t, res)
			require.True(t, res.IsError)
			assert.Contains(t, strings.ToLower(extractTextFromContent(t, res.Content)), "temporarily unavailable")
			after, err := os.ReadFile(fixture.configPath)
			require.NoError(t, err)
			assert.Equal(t, before, after, "readiness rejection mutated config")
			assert.Zero(t, registers.Load())
			assert.Zero(t, launches.Load())
			assertTrackInFlightEmpty(t, fixture.srv, fixture.repoRoot)
		})
	}
}

func TestTrackRepositoryNilLifecycleDoesNotClaimInflight(t *testing.T) {
	fixture := newTrackGuardFixture(t, "ready")
	if fixture.srv.lifecycle != nil {
		require.NoError(t, fixture.srv.lifecycle.Close())
		fixture.srv.lifecycle = nil
	}
	untracked := t.TempDir()
	var launches atomic.Int32

	res, err := fixture.srv.handleTrackRepositoryWithRuntime(
		context.Background(),
		trackGuardRequest(untracked),
		trackRepositoryRuntime{launch: func(func()) { launches.Add(1) }},
	)
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, extractTextFromContent(t, res.Content), "checkout lifecycle is not wired")
	assert.Zero(t, launches.Load())
	assertTrackInFlightEmpty(t, fixture.srv, untracked)
}

func TestTrackRepositoryReadyDuplicateIsSynchronousNoOp(t *testing.T) {
	fixture := newTrackGuardFixture(t, "ready")
	before, err := os.ReadFile(fixture.configPath)
	require.NoError(t, err)
	var registers atomic.Int32
	var launches atomic.Int32

	res, err := fixture.srv.handleTrackRepositoryWithRuntime(
		context.Background(),
		trackGuardRequest(fixture.repoRoot),
		trackRepositoryRuntime{
			register: func(context.Context, config.RepoEntry, store_sqlite.IntentSourceKind) (indexer.RegisterResult, error) {
				registers.Add(1)
				return indexer.RegisterResult{}, nil
			},
			launch: func(run func()) {
				launches.Add(1)
				run()
			},
		},
	)
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Contains(t, extractTextFromContent(t, res.Content), "already tracked")
	assert.Zero(t, registers.Load())
	assert.Zero(t, launches.Load())
	after, err := os.ReadFile(fixture.configPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assertTrackInFlightEmpty(t, fixture.srv, fixture.repoRoot)
}

func TestTrackRepositoryReadyCatalogWithoutProcessIndexerRegistersOnce(t *testing.T) {
	fixture := newTrackGuardFixture(t, "ready")
	emptyIndexer := indexer.NewMultiIndexer(
		fixture.store,
		testRegistry(),
		search.NewNull(),
		fixture.configManager,
		zap.NewNop(),
	)
	t.Cleanup(func() { _ = emptyIndexer.Close(context.Background()) })
	fixture.srv.multiIndexer = emptyIndexer
	require.Nil(t, emptyIndexer.GetIndexer(trackGuardPrefix))
	var registers atomic.Int32
	var launches atomic.Int32

	res, err := fixture.srv.handleTrackRepositoryWithRuntime(
		context.Background(),
		trackGuardRequest(fixture.repoRoot),
		trackRepositoryRuntime{
			register: func(context.Context, config.RepoEntry, store_sqlite.IntentSourceKind) (indexer.RegisterResult, error) {
				registers.Add(1)
				return indexer.RegisterResult{AlreadyTracked: true, Prefix: trackGuardPrefix}, nil
			},
			launch: func(run func()) {
				launches.Add(1)
				run()
			},
		},
	)
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Contains(t, extractTextFromContent(t, res.Content), "already tracked")
	assert.EqualValues(t, 1, registers.Load())
	assert.EqualValues(t, 1, launches.Load())
	assertTrackInFlightEmpty(t, fixture.srv, fixture.repoRoot)
}

func TestTrackRepositoryConcurrentDuplicatesCoalesce(t *testing.T) {
	fixture := newTrackGuardFixture(t, "ready")
	untracked := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	var registers atomic.Int32
	var launches atomic.Int32
	var workers sync.WaitGroup
	runtime := trackRepositoryRuntime{
		register: func(context.Context, config.RepoEntry, store_sqlite.IntentSourceKind) (indexer.RegisterResult, error) {
			registers.Add(1)
			close(started)
			<-release
			return indexer.RegisterResult{AlreadyTracked: true, Prefix: "new-repo"}, nil
		},
		launch: func(run func()) {
			launches.Add(1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				run()
			}()
		},
	}
	type callResult struct {
		res *mcplib.CallToolResult
		err error
	}
	firstDone := make(chan callResult, 1)
	go func() {
		res, err := fixture.srv.handleTrackRepositoryWithRuntime(
			context.Background(), trackGuardRequest(untracked), runtime)
		firstDone <- callResult{res: res, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first registration did not start")
	}

	second, err := fixture.srv.handleTrackRepositoryWithRuntime(
		context.Background(), trackGuardRequest(untracked), runtime)
	require.NoError(t, err)
	require.False(t, second.IsError)
	assert.Contains(t, extractTextFromContent(t, second.Content), "accepted")
	close(release)
	first := <-firstDone
	require.NoError(t, first.err)
	require.False(t, first.res.IsError)
	workers.Wait()
	assert.EqualValues(t, 1, registers.Load())
	assert.EqualValues(t, 1, launches.Load())
	assertTrackInFlightEmpty(t, fixture.srv, untracked)
}

func BenchmarkTrackRepositoryUnavailable(b *testing.B) {
	fixture := newTrackGuardFixture(b, "warming")
	req := trackGuardRequest(fixture.repoRoot)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := fixture.srv.handleTrackRepository(context.Background(), req)
		if err != nil || res == nil || !res.IsError {
			b.Fatalf("unavailable track result = %#v, %v", res, err)
		}
	}
}

func BenchmarkTrackRepositoryReadyDuplicateNoOp(b *testing.B) {
	fixture := newTrackGuardFixture(b, "ready")
	req := trackGuardRequest(fixture.repoRoot)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := fixture.srv.handleTrackRepository(context.Background(), req)
		if err != nil || res == nil || res.IsError {
			b.Fatalf("duplicate track result = %#v, %v", res, err)
		}
	}
}
