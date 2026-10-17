//go:build performance

package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

// Run only through the explicit performance/benchmark flow. The measured
// interval is production capture, cache proof, extraction, payload and publish;
// foreground save/accept, warmup and selected-route assertions are outside the
// timer. Successful capture/publication/cache invariant guards inside run are
// included; this is attachment-job timing with those harness checks.
func BenchmarkContractDeltaJobRouteEdit(b *testing.B) {
	b.Setenv("GORTEX_ASYNC_CONTRACTS", "")
	ctx := b.Context()
	root := b.TempDir()
	write := func(name, body string) {
		require.NoError(b, os.WriteFile(filepath.Join(root, name), []byte(body), 0o600))
	}
	write(".gortex.yaml", "semantic:\n  enabled: false\n")
	write("go.mod", "module example.test/contractbench\n\ngo 1.22\n")
	for i := 0; i < 200; i++ {
		write(fmt.Sprintf("filler%03d.go", i), fmt.Sprintf("package fixture\nfunc Filler%d() int { return %d }\n", i, i))
	}
	routeSource := func(route string) string {
		return "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET " + route + "\", handle) }\nfunc handle(w http.ResponseWriter, r *http.Request) {}\n"
	}
	write("routes.go", routeSource("/warm"))
	store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "source.sqlite"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	cfg := &config.GlobalConfig{Repos: []config.RepoEntry{{Name: "fixture", Path: root}}}
	configPath := filepath.Join(b.TempDir(), "config.yaml")
	cfg.SetConfigPath(configPath)
	require.NoError(b, cfg.Save())
	manager, err := config.NewConfigManager(configPath)
	require.NoError(b, err)
	registry := newTestRegistry()
	mi := NewMultiIndexer(store, registry, search.NewNull(), manager, zap.NewNop())
	b.Cleanup(func() { require.NoError(b, mi.Close(context.Background())) })
	mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	results, err := mi.IndexAll()
	require.NoError(b, err)
	require.Empty(b, results["fixture"].FailedFiles)
	materializer := &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: graphview.NewLeaseManager()}
	options := ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: materializer, MultiIndexer: mi, Registry: registry, Config: config.Default().Index, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() }}
	require.NoError(b, reconcilePrimaryContractBaseline(ctx, options, "fixture"))
	var cache *contractDeltaCache
	sequence := 0
	run := func() ContractFollowupReport {
		sequence++
		inputs, err := materializer.CaptureContractInputs(ctx, nil, "fixture", "")
		require.NoError(b, err)
		snapshot, targets, err := NewContractFollowupCapture(options)(ctx, nil, inputs, "fixture", "")
		require.NoError(b, err)
		// Capture owns resources before the reservation/session assertions.
		// The worker normally consumes this handoff; the deferred fallback
		// closes it on Fatal too. Both doors share one idempotent release.
		inner := snapshot.Release
		var released sync.Once
		snapshot.Release = func() {
			released.Do(func() {
				if inner != nil {
					inner()
				}
			})
		}
		defer snapshot.Release()
		require.NotNil(b, inner, "successful capture must provide its owned handoff")
		require.Len(b, targets, 1)
		snapshot.Work = targets[0].Work
		_, payload, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "contract-route-benchmark", LayerID: fmt.Sprint(sequence), GenerationKind: "contract_analysis", CreatedAt: int64(sequence)})
		require.NoError(b, err)
		session, err := newContractDeltaSession(cache, snapshot, options.Config)
		require.NoError(b, err)
		report, err := RunContractFollowup(ctx, ContractFollowupRequest{RebuildReason: ContractFollowupUnknownInterval, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: registry, Config: options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: materializer.Leases, Logger: zap.NewNop(), Yield: options.Yield, delta: session})
		require.NoError(b, err)
		require.True(b, report.Published)
		cache = session.cache
		require.NotNil(b, cache)
		require.True(b, cache.complete)
		return report
	}
	save := func(route string) {
		write("routes.go", routeSource(route))
		path := filepath.Join(root, "routes.go")
		future := time.Now().Add(time.Duration(sequence+1) * time.Minute)
		require.NoError(b, os.Chtimes(path, future, future))
		result, err := mi.IncrementalReindexRepo("fixture", []string{path})
		require.NoError(b, err)
		require.Empty(b, result.FailedFiles)
	}
	run()
	previous := "/warm"
	files := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Use real extraction to refill, rather than rewriting the refresh
		// counter. Thus every timed job remains below the periodic reset.
		if i > 0 && i%128 == 0 {
			cache = nil
			previous = fmt.Sprintf("/refill-%d", i)
			save(previous)
			run()
		}
		route := fmt.Sprintf("/edit-%d", i)
		save(route)
		b.StartTimer()
		report := run()
		b.StopTimer()
		require.Equal(b, viewmetrics.ContractModeDelta, report.Mode)
		require.Equal(b, ContractFollowupChangedBoundary, report.RebuildReason)
		require.Contains(b, report.ChangedFiles, "fixture/routes.go")
		files += report.Files
		func() {
			inputs, err := materializer.CaptureContractInputs(ctx, nil, "fixture", "")
			require.NoError(b, err)
			analysis, err := materializer.OpenContractAnalysisForInputs(ctx, nil, inputs)
			require.NoError(b, err)
			defer analysis.Close() // semantic checks/Fatal stay outside the timer
			projection, err := analysis.Layer.LayerContractIDProjectionContext(ctx, []string{"http::GET::" + route, "http::GET::" + previous})
			require.NoError(b, err)
			require.NotNil(b, projection.Targets["http::GET::"+route])
			require.Nil(b, projection.Targets["http::GET::"+previous])
			require.NoError(b, analysis.Validate(ctx))
		}()
		previous = route
	}
	b.ReportMetric(float64(files)/float64(b.N), "extracted_files/op")
	b.ReportMetric(1, "delta_jobs/op")
}
