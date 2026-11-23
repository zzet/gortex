package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

type watcherRelationRebindObservation struct {
	applied bool
	gateErr error
}

// Every operation uses the real SQLite store. Hold its writer gate only for
// the first application of the exact rebind selected by the real resolver.
type watcherRelationRebindStore struct {
	*store_sqlite.Store
	ctx   context.Context
	deny  bool
	first chan watcherRelationRebindObservation
	calls atomic.Int32
}

func (s *watcherRelationRebindStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.ReindexEdgesApplied(batch)
}

func (s *watcherRelationRebindStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	for _, row := range batch {
		if row.Edge == nil || row.Edge.From != "main.go::NewCaller" || row.Edge.Kind != graph.EdgeCalls ||
			row.OldTo != graph.UnresolvedMarker+"Target" || row.Edge.To != "main.go::Target" {
			continue
		}
		if s.calls.Add(1) != 1 {
			break
		}
		if s.deny {
			release, err := s.Store.HoldWriteGate(s.ctx)
			if err != nil {
				s.first <- watcherRelationRebindObservation{gateErr: err}
				return false
			}
			defer release()
		}
		applied := s.Store.ReindexEdgesApplied(batch)
		s.first <- watcherRelationRebindObservation{applied: applied}
		return applied
	}
	return s.Store.ReindexEdgesApplied(batch)
}

func TestWatcherRelationTierRequiresAppliedRebind(t *testing.T) {
	for _, tc := range []struct {
		name string
		deny bool
	}{
		{name: "applied"},
		{name: "writer_gate_refused", deny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "main.go")
			const before = "package main\nfunc Target() {}\n"
			const after = before + "func NewCaller() { Target() }\n"
			require.NoError(t, os.WriteFile(path, []byte(before), 0600))
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "relations.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			registry := parser.NewRegistry()
			registry.Register(languages.NewGoExtractor())
			idx := New(store, registry, config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			_, err = idx.Index(root)
			require.NoError(t, err)
			require.NotNil(t, store.GetNode("main.go::Target"))
			require.Nil(t, store.GetNode("main.go::NewCaller"))
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			probe := &watcherRelationRebindStore{Store: store, ctx: ctx, deny: tc.deny, first: make(chan watcherRelationRebindObservation, 1)}
			idx.resolver.SetGraph(probe)
			watcher, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
			require.NoError(t, err)
			// Exclude native save echoes. The explicit owned save still uses
			// the real admission, incremental resolver and publication paths.
			require.NoError(t, watcher.Start([]string{t.TempDir()}))
			t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
			require.NoError(t, os.WriteFile(path, []byte(after), 0600))
			ticket, err := watcher.EnqueueToolMutation(ctx, path)
			require.NoError(t, err)
			var result MutationResult
			select {
			case result = <-ticket.Done:
			case <-ctx.Done():
				t.Fatal("owned mutation did not complete")
			}
			var observation watcherRelationRebindObservation
			select {
			case observation = <-probe.first:
			default:
				t.Fatal("real resolver did not select the exact caller rebind")
			}
			require.NoError(t, observation.gateErr)
			require.Equal(t, !tc.deny, observation.applied)
			require.NoError(t, ctx.Err(), "global request cancellation is outside the fixture")
			physical, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, after, string(physical))
			edges := store.GetOutEdges("main.go::NewCaller")
			require.Len(t, edges, 1)
			state := ticket.TierState()
			t.Logf("matching rebind applied=%t attempts=%d ticket=%d applied=%d error=%v target=%q tiers=%+v",
				observation.applied, probe.calls.Load(), ticket.Generation, result.AppliedGeneration, result.Err, edges[0].To, state)
			if edges[0].To == "main.go::Target" {
				require.NoError(t, result.Err)
				require.GreaterOrEqual(t, result.AppliedGeneration, ticket.Generation)
				require.False(t, state.RelationsAt.IsZero())
				require.False(t, state.Failed || state.Partial)
				return
			}
			require.True(t, tc.deny, "healthy application must resolve the actual call")
			require.Equal(t, graph.UnresolvedMarker+"Target", edges[0].To)
			require.True(t, state.RelationsAt.IsZero(), "a denied SQLite rebind must leave relations unobserved")
		})
	}
}
