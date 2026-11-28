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

// Every operation uses the real SQLite store. Hold its writer gate on the
// exact Target rebind selected by the real resolver (either once for transient
// recovery or continuously while deny is true).
type watcherRelationRebindStore struct {
	*store_sqlite.Store
	ctx                context.Context
	deny               atomic.Bool
	denyOnce           atomic.Bool
	first              chan watcherRelationRebindObservation
	calls              atomic.Int32
	replacementApplied atomic.Bool
}

func (s *watcherRelationRebindStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.ReindexEdgesTryApplied(batch, 0)
}

func (s *watcherRelationRebindStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	return s.ReindexEdgesTryApplied(batch, 0)
}

func (s *watcherRelationRebindStore) ReindexEdgesTryApplied(batch []graph.EdgeReindex, maxWait time.Duration) bool {
	for _, row := range batch {
		if row.Edge == nil || row.Edge.From != "main.go::NewCaller" || row.Edge.Kind != graph.EdgeCalls {
			continue
		}
		if row.OldTo == graph.UnresolvedMarker+"Replacement" && row.Edge.To == "main.go::Replacement" {
			applied := s.Store.ReindexEdgesTryApplied(batch, maxWait)
			s.replacementApplied.Store(applied)
			return applied
		}
		if row.OldTo != graph.UnresolvedMarker+"Target" || row.Edge.To != "main.go::Target" {
			continue
		}
		n := s.calls.Add(1)
		hold := s.deny.Load() || (s.denyOnce.Load() && n == 1)
		if hold {
			release, err := s.HoldWriteGate(s.ctx)
			if err != nil {
				if n == 1 {
					s.first <- watcherRelationRebindObservation{gateErr: err}
				}
				return false
			}
			defer release()
		}
		applied := s.Store.ReindexEdgesTryApplied(batch, maxWait)
		if n == 1 {
			s.first <- watcherRelationRebindObservation{applied: applied}
		}
		return applied
	}
	return s.Store.ReindexEdgesTryApplied(batch, maxWait)
}

func TestWatcherRelationTierRequiresAppliedRebind(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		deny                    bool
		denyOnce                bool
		duplicateBeforeComplete bool
	}{
		{name: "applied"},
		{name: "writer_gate_refused_transient_recovers", denyOnce: true},
		{name: "writer_gate_refused", deny: true},
		{name: "writer_gate_refused_before_completion", deny: true, duplicateBeforeComplete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "main.go")
			const before = "package main\nfunc Target() {}\nfunc Replacement() {}\n"
			const after = before + "func NewCaller() { Target() }\n"
			const repaired = before + "func NewCaller() { Replacement() }\n"
			require.NoError(t, os.WriteFile(path, []byte(before), 0600))
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "relations.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			if tc.deny || tc.denyOnce {
				restoreTimeout := store.SetBusyRetryTimeoutForTest(25 * time.Millisecond)
				t.Cleanup(restoreTimeout)
			}
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
			probe := &watcherRelationRebindStore{Store: store, ctx: ctx, first: make(chan watcherRelationRebindObservation, 1)}
			probe.deny.Store(tc.deny)
			probe.denyOnce.Store(tc.denyOnce)
			idx.resolver.SetGraph(probe)
			watcher, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
			require.NoError(t, err)
			if tc.deny || tc.denyOnce {
				watcher.mutationRetryDelayFn = func(int) time.Duration { return 5 * time.Millisecond }
			}
			beforeComplete := make(chan struct{})
			allowComplete := make(chan struct{})
			releaseCompletion := sync.OnceFunc(func() { close(allowComplete) })
			if tc.duplicateBeforeComplete {
				watcher.mutationBeforeComplete = func(candidate string, generation uint64) {
					if candidate == path && generation == 1 {
						close(beforeComplete)
						<-allowComplete
					}
				}
			}
			// Exclude native save echoes. The explicit owned save still uses
			// the real admission, incremental resolver and publication paths.
			require.NoError(t, watcher.Start([]string{t.TempDir()}))
			t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
			defer releaseCompletion()
			enqueue := func(path string) *MutationTicket {
				t.Helper()
				ticket, err := watcher.EnqueueToolMutation(ctx, path)
				require.NoError(t, err)
				return ticket
			}
			wait := func(ticket *MutationTicket) MutationResult {
				t.Helper()
				select {
				case result := <-ticket.Done:
					return result
				case <-ctx.Done():
					t.Fatal("owned mutation did not complete")
					return MutationResult{}
				}
			}
			submit := func(path string) (*MutationTicket, MutationResult) {
				t.Helper()
				ticket := enqueue(path)
				return ticket, wait(ticket)
			}
			require.NoError(t, os.WriteFile(path, []byte(after), 0600))
			ticket := enqueue(path)
			var duplicate *MutationTicket
			var duplicateResult MutationResult
			if tc.duplicateBeforeComplete {
				select {
				case <-beforeComplete:
				case <-ctx.Done():
					t.Fatal("refused mutation did not reach the pre-completion boundary")
				}
				require.EqualValues(t, 1, ticket.Generation)
				duplicate, duplicateResult = submit(path)
				releaseCompletion()
			}
			result := wait(ticket)
			var observation watcherRelationRebindObservation
			select {
			case observation = <-probe.first:
			default:
				t.Fatal("real resolver did not select the exact caller rebind")
			}
			require.NoError(t, observation.gateErr)
			require.Equal(t, !tc.deny && !tc.denyOnce, observation.applied)
			require.NoError(t, ctx.Err(), "global request cancellation is outside the fixture")
			physical, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, after, string(physical))
			edges := store.GetOutEdges("main.go::NewCaller")
			require.Len(t, edges, 1)
			state := ticket.TierState()
			t.Logf("matching rebind applied=%t attempts=%d ticket=%d applied=%d error=%v target=%q tiers=%+v",
				observation.applied, probe.calls.Load(), ticket.Generation, result.AppliedGeneration, result.Err, edges[0].To, state)
			if !tc.deny {
				if tc.denyOnce {
					require.GreaterOrEqual(t, probe.calls.Load(), int32(2), "transient refusal must auto-retry and heal")
				}
				require.Equal(t, "main.go::Target", edges[0].To)
				require.NoError(t, result.Err)
				require.GreaterOrEqual(t, result.AppliedGeneration, ticket.Generation)
				require.False(t, state.RelationsAt.IsZero())
				require.False(t, state.DerivedAt.IsZero())
				require.False(t, state.Failed || state.Partial)
				require.False(t, watcher.TierState(path).Partial)
				return
			}
			require.Equal(t, graph.UnresolvedMarker+"Target", edges[0].To)
			require.True(t, state.RelationsAt.IsZero(), "a denied SQLite rebind must leave relations unobserved")
			require.True(t, state.DerivedAt.IsZero())
			require.NoError(t, result.Err, "the structural write already landed")
			require.True(t, result.Reindexed && state.Partial && !state.Failed)
			require.False(t, state.FileGraphAt.IsZero())

			if duplicate == nil {
				duplicate, duplicateResult = submit(path)
			}
			require.NoError(t, duplicateResult.Err)
			require.Greater(t, duplicate.Generation, ticket.Generation)
			require.GreaterOrEqual(t, probe.calls.Load(), int32(6), "same-byte save while pendingResolutionRetry is armed must intentionally retry before degrading")
			t.Logf("same-byte generation=%d applied=%d tiers=%+v", duplicate.Generation, duplicateResult.AppliedGeneration, duplicate.TierState())
			require.True(t, duplicate.TierState().Partial, "sustained contention still carries the checked refusal")
			require.True(t, duplicate.TierState().RelationsAt.IsZero())
			require.True(t, duplicate.TierState().DerivedAt.IsZero())

			other := filepath.Join(root, "other", "other.go")
			require.NoError(t, os.Mkdir(filepath.Dir(other), 0700))
			require.NoError(t, os.WriteFile(other, []byte("package other\nfunc Unrelated() {}\n"), 0600))
			unrelated, unrelatedResult := submit(other)
			require.NoError(t, unrelatedResult.Err)
			require.False(t, unrelated.TierState().Partial)
			require.False(t, unrelated.TierState().RelationsAt.IsZero())
			require.True(t, watcher.TierState(path).Partial, "an unrelated file under sustained contention must not clear the owned deferral")

			// Once sustained contention clears, a clean save on other/other.go
			// opportunistically heals main.go and clears its Partial tier via onRepairedFiles.
			probe.deny.Store(false)
			require.NoError(t, os.WriteFile(other, []byte("package other\nfunc Unrelated() int { return 1 }\n"), 0600))
			healer, healerResult := submit(other)
			require.NoError(t, healerResult.Err)
			require.False(t, healer.TierState().Partial)
			require.False(t, healer.TierState().RelationsAt.IsZero())
			healedEdges := store.GetOutEdges("main.go::NewCaller")
			require.Len(t, healedEdges, 1)
			require.Equal(t, "main.go::Target", healedEdges[0].To, "clean unrelated save must opportunistically heal queued main.go retry")
			require.False(t, watcher.TierState(path).Partial, "opportunistic repair must clear watcher failedPaths via onRepairedFiles")

			require.NoError(t, os.WriteFile(path, []byte(repaired), 0600))
			healthy, healthyResult := submit(path)
			require.NoError(t, healthyResult.Err)
			require.NoError(t, ctx.Err())
			require.Greater(t, healthy.Generation, duplicate.Generation)
			require.True(t, probe.replacementApplied.Load(), "the new generation must land its own checked rebind")
			require.False(t, healthy.TierState().Partial)
			require.False(t, healthy.TierState().RelationsAt.IsZero())
			require.False(t, watcher.TierState(path).Partial)
			resolved := store.GetOutEdges("main.go::NewCaller")
			require.Len(t, resolved, 1)
			require.Equal(t, "main.go::Replacement", resolved[0].To)
			require.True(t, duplicate.TierState().RelationsAt.IsZero(), "the newer generation cannot rewrite the old ticket")
			t.Logf("owned duplicate generation=%d remains partial, unrelated generation=%d served, healthy generation=%d rebind applied",
				duplicate.Generation, unrelated.Generation, healthy.Generation)
		})
	}
}

func TestWatcherStormMutation_SustainedReindexRefusalDegradesCleanly(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "main.go")
	auxPath := filepath.Join(root, "aux.go")
	const before = "package main\nfunc Target() {}\n"
	const after = before + "func NewCaller() { Target() }\n"
	require.NoError(t, os.WriteFile(mainPath, []byte(before), 0600))
	require.NoError(t, os.WriteFile(auxPath, []byte("package main\nfunc Aux() {}\n"), 0600))

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "storm-relations.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	restoreTimeout := store.SetBusyRetryTimeoutForTest(25 * time.Millisecond)
	t.Cleanup(restoreTimeout)

	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := New(store, registry, config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	_, err = idx.Index(root)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	probe := &watcherRelationRebindStore{Store: store, ctx: ctx, first: make(chan watcherRelationRebindObservation, 1)}
	probe.deny.Store(true)
	idx.resolver.SetGraph(probe)

	watcher, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 5, StormThreshold: 1, StormQuietPeriodMs: 5}, zap.NewNop())
	require.NoError(t, err)
	watcher.mutationRetryDelayFn = func(int) time.Duration { return 5 * time.Millisecond }
	require.NoError(t, watcher.Start([]string{t.TempDir()}))
	t.Cleanup(func() { require.NoError(t, watcher.Stop()) })

	require.NoError(t, os.WriteFile(mainPath, []byte(after), 0600))
	require.NoError(t, os.WriteFile(auxPath, []byte("package main\nfunc Aux() int { return 1 }\n"), 0600))
	tickets, err := watcher.EnqueueFileMutations(ctx, []string{mainPath, auxPath})
	require.NoError(t, err)
	require.Len(t, tickets, 2)

	for p, tk := range tickets {
		select {
		case res := <-tk.Done:
			require.NoError(t, res.Err, "sustained storm refusal on %s must degrade cleanly after maxPointRebindRetries", p)
			require.True(t, res.Reindexed)
			ts := tk.TierState()
			require.True(t, ts.Partial, "degraded storm ticket for %s must report Partial=true", p)
			require.True(t, ts.RelationsAt.IsZero(), "degraded storm ticket for %s must leave RelationsAt zero", p)
			require.True(t, ts.DerivedAt.IsZero(), "degraded storm ticket for %s must leave DerivedAt zero", p)
		case <-ctx.Done():
			t.Fatalf("storm mutation ticket for %s hung under sustained ErrReindexWriteRefused", p)
		}
	}
	require.GreaterOrEqual(t, probe.calls.Load(), int32(maxPointRebindRetries+1), "storm drain must attempt initial pass + maxPointRebindRetries retries")

	watcher.stormMu.Lock()
	stormStillActive := watcher.stormActive
	watcher.stormMu.Unlock()
	require.False(t, stormStillActive, "stormActive must be false after graceful degradation")
	require.True(t, idx.hasPendingResolutionRetry(), "degraded storm must retain pendingResolutionRetry for later recovery")
}

func TestMutationTiers_MarkRelationsPreservesNonRefusalPartial(t *testing.T) {
	t.Run("deferred_and_refused_preserves_partial_and_stamps_relations", func(t *testing.T) {
		path := filepath.Clean("/repo/deferred.go")
		tk := newMutationTierTicket(path, 1, make(chan MutationResult, 1))
		w := &Watcher{
			mutationWaiters: map[string]map[uint64]mutationWaiter{
				path: {1: {ticket: tk, done: make(chan MutationResult, 1)}},
			},
		}
		w.markDeferredMutationTiers(map[string]uint64{path: 1})
		w.markRefusedRelationTiers(path, 1, false)
		require.True(t, w.failedPaths[path].rebindRefused)
		require.True(t, w.failedPaths[path].nonRefusalPartial)
		require.True(t, w.failedPaths[path].RelationsAt.IsZero())

		w.markRelations(path, 1, 1)

		waiterState := tk.TierState()
		require.True(t, waiterState.Partial, "non-refusal deferral must keep Partial=true after refusal heals")
		require.False(t, waiterState.rebindRefused)
		require.True(t, waiterState.DerivedAt.IsZero())
		require.False(t, waiterState.RelationsAt.IsZero(), "healed refusal must stamp RelationsAt")

		failedState, exists := w.failedPaths[path]
		require.True(t, exists, "non-refusal Partial entry must remain in failedPaths")
		require.True(t, failedState.Partial)
		require.False(t, failedState.rebindRefused)
		require.True(t, failedState.DerivedAt.IsZero())
		require.False(t, failedState.RelationsAt.IsZero(), "retained non-failed entry must stamp RelationsAt")
	})

	t.Run("refusal_only_clears_partial_and_failed_path", func(t *testing.T) {
		path := filepath.Clean("/repo/refusal_only.go")
		tk := newMutationTierTicket(path, 1, make(chan MutationResult, 1))
		w := &Watcher{
			mutationWaiters: map[string]map[uint64]mutationWaiter{
				path: {1: {ticket: tk, done: make(chan MutationResult, 1)}},
			},
		}
		w.markRefusedRelationTiers(path, 1, false)
		w.markRelations(path, 1, 1)

		waiterState := tk.TierState()
		require.False(t, waiterState.Partial)
		require.False(t, waiterState.rebindRefused)
		require.False(t, waiterState.RelationsAt.IsZero())
		_, exists := w.failedPaths[path]
		require.False(t, exists, "refusal-only entry must retire from failedPaths once repaired")
	})
}

func TestMutationTiers_MarkRepairedFailedPathsInterleavesWithCaptureTierRepair(t *testing.T) {
	t.Run("newer_parse_failure_interleaves_with_capture_tier_repair", func(t *testing.T) {
		path := filepath.Clean("/repo/parse_fail.go")
		tk1 := newMutationTierTicket(path, 1, nil)
		w := &Watcher{
			mutationWaiters: map[string]map[uint64]mutationWaiter{
				path: {1: {ticket: tk1}},
			},
		}
		w.markRefusedRelationTiers(path, 1, false)
		delete(w.mutationWaiters, path)

		tk2 := newMutationTierTicket(path, 2, nil)
		w.mu.Lock()
		clean := w.finishMutationTierLocked(path, tk2, errors.New("parse failed"), DerivedFanoutCompleteness{})
		w.mu.Unlock()
		require.False(t, clean)
		require.True(t, w.failedPaths[path].Failed)
		require.False(t, w.failedPaths[path].Partial)
		require.True(t, w.failedPaths[path].rebindRefused)

		repair := w.CaptureTierRepair(path)
		require.NotNil(t, repair)

		w.markRepairedFailedPaths([]string{path}, false)
		st, exists := w.failedPaths[path]
		require.True(t, exists, "markRepairedFailedPaths must not delete a Failed entry")
		require.True(t, st.Failed)
		require.False(t, st.rebindRefused)
		require.True(t, st.RelationsAt.IsZero(), "Failed entry must not be stamped with RelationsAt")

		repair()
		_, exists = w.failedPaths[path]
		require.False(t, exists, "CaptureTierRepair closure must clear Failed entry despite concurrent rebindRefused clear")
	})

	t.Run("attempt0_refused_then_attempt1_parse_failed", func(t *testing.T) {
		path := filepath.Clean("/repo/retry_parse_fail.go")
		tk := newMutationTierTicket(path, 1, nil)
		w := &Watcher{
			mutationWaiters: map[string]map[uint64]mutationWaiter{
				path: {1: {ticket: tk}},
			},
		}
		w.markRefusedRelationTiers(path, 1, false)
		w.mu.Lock()
		clean := w.finishMutationTierLocked(path, tk, errors.New("parse failed on retry"), DerivedFanoutCompleteness{})
		delete(w.mutationWaiters, path)
		w.mu.Unlock()
		require.False(t, clean)

		st := w.failedPaths[path]
		require.True(t, st.Failed)
		require.True(t, st.Partial)
		require.True(t, st.rebindRefused)
		require.False(t, st.nonRefusalPartial)
		require.Nil(t, w.CaptureTierRepair(path), "CaptureTierRepair must return nil while Partial is true")

		w.markRepairedFailedPaths(nil, true)
		st, exists := w.failedPaths[path]
		require.True(t, exists)
		require.True(t, st.Failed)
		require.False(t, st.rebindRefused)
		require.False(t, st.Partial, "clearing refusal-only Partial must unblock CaptureTierRepair")

		repair := w.CaptureTierRepair(path)
		require.NotNil(t, repair)
		repair()
		_, exists = w.failedPaths[path]
		require.False(t, exists)
	})
}

func TestMutationTiers_MarkRefusedRelationTiersDuplicatePreservesNonRefusalPartial(t *testing.T) {
	path := filepath.Clean("/repo/dup.go")
	tk1 := newMutationTierTicket(path, 1, nil)
	w := &Watcher{
		mutationWaiters: map[string]map[uint64]mutationWaiter{
			path: {1: {ticket: tk1}},
		},
	}
	w.markDeferredMutationTiers(map[string]uint64{path: 1})
	w.markRefusedRelationTiers(path, 1, false)
	delete(w.mutationWaiters, path)
	require.True(t, w.failedPaths[path].nonRefusalPartial)

	tk2 := newMutationTierTicket(path, 2, nil)
	w.mutationWaiters[path] = map[uint64]mutationWaiter{2: {ticket: tk2}}
	w.markRefusedRelationTiers(path, 2, true)
	require.True(t, tk2.TierState().nonRefusalPartial)
	require.True(t, w.failedPaths[path].nonRefusalPartial)

	w.markRelations(path, 2, 2)
	require.True(t, tk2.TierState().Partial)
	require.False(t, tk2.TierState().RelationsAt.IsZero())
	require.True(t, w.failedPaths[path].Partial)
	require.False(t, w.failedPaths[path].RelationsAt.IsZero())
}

func TestMutationTiers_CoalescedCleanGenRetiresOlderNonRefusalPartial(t *testing.T) {
	path := filepath.Clean("/repo/coalesce.go")
	fanout := DerivedFanoutCompleteness{Observed: true, Complete: true}
	for i := 0; i < 32; i++ {
		tk1 := newMutationTierTicket(path, 1, nil)
		tk1.tiers.Partial = true
		tk1.tiers.nonRefusalPartial = true
		tk2 := newMutationTierTicket(path, 2, nil)
		w := &Watcher{
			pendingGeneration:          map[string]uint64{path: 2},
			observedMutationGeneration: map[string]uint64{path: 2},
			mutationWaiters: map[string]map[uint64]mutationWaiter{
				path: {
					1: {ticket: tk1, done: make(chan MutationResult, 1)},
					2: {ticket: tk2, done: make(chan MutationResult, 1)},
				},
			},
		}
		ok, _ := w.completeMutationWaitersObserved(path, 2, nil, fanout, false, nil)
		require.True(t, ok)
		_, exists := w.failedPaths[path]
		require.False(t, exists, "point completion iteration %d: clean Gen 2 must deterministically retire Gen 1 nonRefusalPartial", i)

		tkStorm1 := newMutationTierTicket(path, 1, nil)
		tkStorm1.tiers.Partial = true
		tkStorm1.tiers.nonRefusalPartial = true
		tkStorm2 := newMutationTierTicket(path, 2, nil)
		wStorm := &Watcher{
			pendingGeneration:          map[string]uint64{path: 2},
			observedMutationGeneration: map[string]uint64{path: 2},
			mutationWaiters: map[string]map[uint64]mutationWaiter{
				path: {
					1: {ticket: tkStorm1, done: make(chan MutationResult, 1)},
					2: {ticket: tkStorm2, done: make(chan MutationResult, 1)},
				},
			},
		}
		wStorm.completeStormMutationWaiters(map[string]uint64{path: 2}, &IndexResult{}, nil, fanout)
		_, exists = wStorm.failedPaths[path]
		require.False(t, exists, "storm completion iteration %d: clean Gen 2 must deterministically retire Gen 1 nonRefusalPartial", i)
	}
}
