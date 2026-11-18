package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestWarmupDeltaFrontierTrackedNpmOfflineEdit(t *testing.T) {
	for _, providerFirst := range []bool{true, false} {
		t.Run(strconv.FormatBool(providerFirst), func(t *testing.T) {
			dir := t.TempDir()
			provider, consumer := filepath.Join(dir, "provider"), filepath.Join(dir, "consumer")
			write := func(rel, body string) {
				t.Helper()
				p := filepath.Join(dir, rel)
				require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
				require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
			}
			write("provider/package.json", `{"name":"p","exports":"./src/index.ts"}`)
			write("provider/src/index.ts", "export function f(): number { return 42; }\n")
			for _, n := range []string{"a", "b", "c", "d"} {
				write("provider/src/"+n+".ts", "export function h"+n+"(): number { return 1; }\n")
			}
			write("consumer/package.json", `{"name":"app","dependencies":{"p":"^1"}}`)
			write("consumer/src/app.ts", "import { f } from 'p';\nexport function run(): number { return f(); }\n")
			entries := []config.RepoEntry{{Path: provider, Name: "provider", Workspace: "shared"}, {Path: consumer, Name: "consumer", Workspace: "shared"}}
			if !providerFirst {
				entries[0], entries[1] = entries[1], entries[0]
			}
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			gc := &config.GlobalConfig{Repos: entries}
			gc.SetConfigPath(cfgPath)
			require.NoError(t, gc.Save())
			cm, err := config.NewConfigManager(cfgPath)
			require.NoError(t, err)
			db := filepath.Join(t.TempDir(), "graph.sqlite")
			sqlite, err := store_sqlite.Open(db)
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlite.Close() })
			var g graph.Store = sqlite
			newIndexer := func() *indexer.MultiIndexer {
				reg := parser.NewRegistry()
				reg.Register(languages.NewTypeScriptExtractor())
				return indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
			}
			mi := newIndexer()
			t.Cleanup(func() { _ = mi.Close(context.Background()) })
			for _, entry := range entries {
				_, err := mi.TrackRepoCtx(context.Background(), entry)
				require.NoError(t, err)
			}
			require.NoError(t, mi.RunPreEnrichResolve(context.Background(), nil, nil))
			check := func(stage string) {
				t.Helper()
				var imports, calls []string
				for _, e := range g.AllEdges() {
					if e.FilePath != "consumer/src/app.ts" {
						continue
					}
					switch e.Kind {
					case graph.EdgeImports:
						imports = append(imports, e.To)
						require.True(t, e.CrossRepo, stage)
					case graph.EdgeCalls:
						calls = append(calls, e.To)
						require.True(t, e.CrossRepo, stage)
					}
				}
				require.ElementsMatch(t, []string{"provider/src/index.ts", "provider/src/index.ts::f"}, imports, stage)
				require.Equal(t, []string{"provider/src/index.ts::f"}, calls, stage)
			}
			check("cold")
			mtimes := map[string]map[string]int64{}
			for _, entry := range entries {
				mtimes[entry.Name] = mi.FileMtimes(entry.Name)
			}
			require.NoError(t, mi.Close(context.Background()))
			require.NoError(t, sqlite.Close())
			// Offline provider edit.
			p := filepath.Join(provider, "src/index.ts")
			require.NoError(t, os.WriteFile(p, []byte("export function f(v = 0): number { return 43 + v; }\n"), 0o644))
			future := time.Now().Add(5 * time.Second)
			require.NoError(t, os.Chtimes(p, future, future))
			sqlite, err = store_sqlite.Open(db)
			require.NoError(t, err)
			g = sqlite
			warm := newIndexer()
			mi = warm
			warm.BeginParallelBatch()
			frontier := newWarmupDeltaFrontier()
			require.NoError(t, warm.RunRepositoryTopologyBatch(context.Background(), func(ctx context.Context) error {
				for _, entry := range entries {
					res, err := warm.ReconcileRepoCtx(ctx, entry, mtimes[entry.Name])
					require.NoError(t, err)
					if res != nil {
						require.False(t, res.FullRetrack)
						if entry.Name == "provider" {
							require.Equal(t, 1, res.StaleFileCount)
							require.Equal(t, []string{"provider/src/index.ts"}, res.DerivedInvalidation.Files)
						}
						frontier.record(res)
					}
				}
				return nil
			}))
			plans, files, exact := frontier.snapshot(2, false)
			require.True(t, exact)
			require.Equal(t, []string{"provider/src/index.ts"}, plans["provider"].Files)
			require.NoError(t, warm.RunPreEnrichResolveFiles(context.Background(), files, nil))
			warm.EndBatch()
			check("after exact warm resolve")
		})
	}
}
