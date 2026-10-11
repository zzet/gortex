package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestInlineFoldPhaseTimingPreservesPublicationAndFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		debug   bool
		copyErr bool
	}{
		{name: "success", debug: true},
		{name: "copy-failure", debug: true, copyErr: true},
		{name: "disabled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			level := zap.InfoLevel
			if test.debug {
				level = zap.DebugLevel
			}
			core, logs := observer.New(level)
			// Install the observer before the coordinator loop is constructed.
			f := newCoordinatorFixtureWithTree(t, builderTreeA())
			gate := NewViewBuildGate()
			gate.Open()
			c := f.coordinatorWithLogger(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: time.Hour, debounceDemand: true}, zap.New(core))
			c.compaction.mu.Lock()
			c.compaction.closed = true
			c.compaction.mu.Unlock()
			require.NoError(t, c.reconcile(context.Background()).Err)
			l := &CheckoutLifecycle{catalog: f.catalog, store: f.store, coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
			mcpChainAtTheCap(t, f, l)
			logs.TakeAll()
			if test.copyErr {
				c.compaction.mu.Lock()
				c.compaction.copyHook = func(context.Context, int64) error {
					return errors.New("phase-test copy refused")
				}
				c.compaction.mu.Unlock()
			}
			at := mcpEdit(t, l, f, func() { chainBurstEdit(t, f, maxChainWalkDepth) })
			require.NoError(t, c.waitDirtyChainCompactions(context.Background()))
			if test.copyErr {
				require.Zero(t, at.DirtyParentGenerationID, "a refused copy still builds direct")
				require.Equal(t, 1, at.DirtyChainDepth)
			} else {
				require.Positive(t, at.DirtyParentGenerationID, "checked copies still become the selected parent")
				require.True(t, at.DirtyParentPreferred)
				require.Equal(t, 2, at.DirtyChainDepth)
			}
			entries := logs.FilterMessage("checkout coordinator: inline fold phases").All()
			if !test.debug {
				require.Empty(t, entries, "disabled debug diagnostics install no fold observer")
				return
			}
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			want := []string{"planning", "manifest_read", "reservation", "copy"}
			if test.copyErr {
				want = append(want, "cleanup")
				require.Equal(t, "copy", fields["failed_stage"])
				require.Equal(t, fields["phase_fold_copy_ms"], fields["failed_stage_elapsed_ms"])
				require.Contains(t, fields["error"], "phase-test copy refused")
			} else {
				want = append(want, "manifest_write", "exact_validation", "derivation_stamps", "publish_finish")
				require.Equal(t, "", fields["failed_stage"])
				require.NotContains(t, fields, "failed_stage_elapsed_ms")
				require.NotContains(t, fields, "phase_fold_cleanup_ms")
			}
			var got []string
			for _, field := range entries[0].Context {
				for _, phase := range want {
					if field.Key == "phase_fold_"+phase+"_ms" {
						got = append(got, phase)
						ms, ok := fields[field.Key].(float64)
						require.True(t, ok, "phase times use numeric milliseconds")
						require.GreaterOrEqual(t, ms, float64(0))
					}
				}
			}
			require.Equal(t, want, got, "exclusive phase order follows the existing operations")
		})
	}
}
