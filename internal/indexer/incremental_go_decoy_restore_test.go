package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestIncrementalGoProducerWithdrawalAndRestoreRejectsNestedDecoy(t *testing.T) {
	root := t.TempDir()
	producerPath := filepath.Join(root, "producer", "producer.go")
	producerSource := "package producer\nfunc Produce() int { return 7 }\n"
	files := map[string]string{
		"go.mod":                     "module example.test/app\n\ngo 1.24\n",
		"consumer/consumer.go":       "package consumer\nimport \"example.test/app/producer\"\nfunc Consume() int { return producer.Produce() }\n",
		"producer/producer.go":       producerSource,
		"decoy/go.mod":               "module example.test/decoy\n\ngo 1.24\n",
		"decoy/producer/producer.go": "package producer\nfunc Produce() int { return 99 }\n",
	}
	for name, body := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700))
		writeFile(t, filepath.Join(root, name), body)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.GlobalConfig{Repos: []config.RepoEntry{{Name: "fixture", Path: root}}}
	cfg.SetConfigPath(configPath)
	require.NoError(t, cfg.Save())
	manager, err := config.NewConfigManager(configPath)
	require.NoError(t, err)
	index := func() (*MultiIndexer, graph.Store) {
		_, store := newSQLiteIndexer(t)
		mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
		t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
		mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
		results, err := mi.IndexAll()
		require.NoError(t, err)
		require.Empty(t, results["fixture"].FailedFiles)
		return mi, store
	}
	mi, store := index()
	caller := "fixture/consumer/consumer.go::Consume"
	producer := "fixture/producer/producer.go::Produce"
	decoy := "fixture/decoy/producer/producer.go::Produce"
	project := func(g graph.Store) []string {
		var rows []string
		for _, edge := range g.GetOutEdges(caller) {
			if edge.Kind == graph.EdgeCalls {
				rows = append(rows, fmt.Sprintf("call %s %s %s %g", edge.To, edge.Origin, edge.Tier, edge.Confidence))
			}
		}
		for _, edge := range g.GetInEdges(caller) {
			if edge.Kind == graph.EdgeValueFlow {
				rows = append(rows, fmt.Sprintf("flow %s %s %s %g", edge.From, edge.Origin, edge.Tier, edge.Confidence))
			}
		}
		slices.Sort(rows)
		return rows
	}
	require.Equal(t, producer, callTargetFrom(t, store, caller))
	require.NotNil(t, store.GetNode(decoy), "the misleading same-named definition must actually exist")
	before := project(store)
	require.Len(t, before, 2, "a positive call and actual returned-value flow make parity nonvacuous")
	writeFile(t, producerPath, "package producer\nfunc Withdrawn() int { return 7 }\n")
	result, err := mi.IncrementalReindexRepo("fixture", []string{producerPath})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.Nil(t, store.GetNode(producer))
	call := callEdgeFrom(t, store, caller)
	require.True(t, graph.IsUnresolvedTarget(call.To), "the cross-repo tail must preserve the per-repo package refusal")
	require.Empty(t, call.Origin)
	require.Zero(t, call.Confidence)
	for _, edge := range store.GetInEdges(caller) {
		if edge.Kind == graph.EdgeValueFlow {
			require.NotEqual(t, decoy, edge.From, "a rejected call must not synthesize a decoy value-flow producer")
		}
	}
	writeFile(t, producerPath, producerSource)
	result, err = mi.IncrementalReindexRepo("fixture", []string{producerPath})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.Equal(t, producer, callTargetFrom(t, store, caller))
	_, cold := index()
	require.Equal(t, before, project(store), "restoring exact source restores the original call and value-flow identities")
	require.Equal(t, project(cold), project(store), "independent cold parse must agree after withdrawal/restoration")
}

// Cold selector resolution and incremental restubbing must agree with the
// actual module/import ownership, including a duplicated tracked module and a
// genuine foreign import. Both source and returned-value identities matter.
func TestIncrementalGoDuplicateModuleAndForeignImportStayColdEquivalent(t *testing.T) {
	for _, tc := range []struct{ name, otherModule, wantRepo string }{
		{"duplicate_module_local", "example.invalid/issue767", "otherrepo"},
		{"foreign_import", "example.invalid/otherrepo", "issue767"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary, other := t.TempDir(), t.TempDir()
			producerSource := "package producer\n\nfunc Produce() int { return 7 }\n"
			consumerSource := "package consumer\n\nimport \"example.invalid/issue767/producer\"\n\nfunc Consume() int { return producer.Produce() }\n"
			for root, files := range map[string]map[string]string{
				primary: {"go.mod": "module example.invalid/issue767\n\ngo 1.24\n", "producer/producer.go": producerSource},
				other:   {"go.mod": "module " + tc.otherModule + "\n\ngo 1.24\n", "producer/producer.go": "package producer\n\nfunc Produce() int { return -1 }\n", "consumer/consumer.go": consumerSource},
			} {
				for name, body := range files {
					path := filepath.Join(root, name)
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
					writeFile(t, path, body)
				}
			}
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			cfg := &config.GlobalConfig{Repos: []config.RepoEntry{{Name: "issue767", Path: primary, Workspace: "fixture"}, {Name: "otherrepo", Path: other, Workspace: "fixture"}}}
			cfg.SetConfigPath(configPath)
			require.NoError(t, cfg.Save())
			manager, err := config.NewConfigManager(configPath)
			require.NoError(t, err)
			index := func() (*MultiIndexer, graph.Store) {
				_, store := newSQLiteIndexer(t)
				mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
				results, err := mi.IndexAll()
				require.NoError(t, err)
				for _, name := range []string{"issue767", "otherrepo"} {
					require.NotNil(t, results[name])
					require.Empty(t, results[name].FailedFiles)
				}
				return mi, store
			}
			mi, store := index()
			caller := "otherrepo/consumer/consumer.go::Consume"
			producer := tc.wantRepo + "/producer/producer.go::Produce"
			project := func(g graph.Store) []string {
				var rows []string
				for _, edge := range g.GetOutEdges(caller) {
					if edge.Kind == graph.EdgeCalls {
						rows = append(rows, fmt.Sprintf("call %s @%s:%d %s %s %g", edge.To, edge.FilePath, edge.Line, edge.Origin, edge.Tier, edge.Confidence))
					}
				}
				for _, edge := range g.GetInEdges(caller) {
					if edge.Kind == graph.EdgeValueFlow {
						rows = append(rows, fmt.Sprintf("flow %s @%s:%d %s %s %g", edge.From, edge.FilePath, edge.Line, edge.Origin, edge.Tier, edge.Confidence))
					}
				}
				slices.Sort(rows)
				return rows
			}
			assertImport := func(g graph.Store) {
				var targets []string
				for _, edge := range g.GetOutEdges("otherrepo/consumer/consumer.go") {
					if edge.Kind == graph.EdgeImports {
						targets = append(targets, edge.To)
					}
				}
				require.Equal(t, []string{tc.wantRepo + "/producer/producer.go"}, targets, "the real import placement independently establishes the expected package")
			}
			assertImport(store)
			require.Equal(t, producer, callTargetFrom(t, store, caller), "cold must bind the actual imported package instance")
			var flows []*graph.Edge
			for _, edge := range store.GetInEdges(caller) {
				if edge.Kind == graph.EdgeValueFlow {
					flows = append(flows, edge)
				}
			}
			require.Len(t, flows, 1)
			require.Equal(t, producer, flows[0].From)
			require.Equal(t, "otherrepo/consumer/consumer.go", flows[0].FilePath)
			require.Equal(t, 5, flows[0].Line)
			before := project(store)
			require.Len(t, before, 2)
			require.NotNil(t, store.GetNode("otherrepo/producer/producer.go::Produce"))
			producerPath := filepath.Join(primary, "producer", "producer.go")
			writeFile(t, producerPath, "package producer\n\nfunc Withdrawn() int { return 7 }\n")
			result, err := mi.IncrementalReindexRepo("issue767", []string{producerPath})
			require.NoError(t, err)
			require.Empty(t, result.FailedFiles)
			require.Nil(t, store.GetNode("issue767/producer/producer.go::Produce"))
			t.Logf("consumer after withdrawal: %v", project(store))
			if tc.wantRepo == "otherrepo" {
				require.Equal(t, before, project(store), "withdrawing another repository must not retarget a local imported call")
			} else {
				require.True(t, graph.IsUnresolvedTarget(callTargetFrom(t, store, caller)))
				for _, edge := range store.GetInEdges(caller) {
					if edge.Kind == graph.EdgeValueFlow {
						require.NotEqual(t, "issue767/producer/producer.go::Produce", edge.From)
						require.NotEqual(t, "otherrepo/producer/producer.go::Produce", edge.From)
					}
				}
			}
			writeFile(t, producerPath, producerSource)
			result, err = mi.IncrementalReindexRepo("issue767", []string{producerPath})
			require.NoError(t, err)
			require.Empty(t, result.FailedFiles)
			t.Logf("consumer after restoration: %v", project(store))
			require.Equal(t, producer, callTargetFrom(t, store, caller), "restored call binding is a separate premise from returned-value coverage")
			_, cold := index()
			assertImport(cold)
			require.Equal(t, producer, callTargetFrom(t, cold, caller))
			require.Equal(t, before, project(cold), "independent current cold establishes the unchanged-source oracle")
			require.Equal(t, project(cold), project(store), "call and exact-site returned-value flow must both survive restoration")
		})
	}
}
