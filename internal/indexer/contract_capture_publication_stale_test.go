package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Use the real read-side refusal after a primary input moves. Only the
// publication-capture boundary marks it stale; an unrelated raw catalog
// guard remains a terminal failure. The old promise must resolve so a
// request can bring a new capture instead of falling back immediately.
func TestContractSchedulerPublicationCaptureMovementAllowsNewCapture(t *testing.T) {
	for _, stale := range []bool{true, false} {
		name := "unrelated_guard"
		if stale {
			name = "moved_capture"
		}
		t.Run(name, func(t *testing.T) {
			p := newSchedulerProbe(t, nil)
			ctx := context.Background()
			state := graph.ContractInputState{RepoPrefix: "r", InputVersion: "v", InputFingerprint: "old", Accepted: true}
			require.NoError(t, p.f.store.BeginContractInputMutationContext(ctx, nil, state, nil))
			require.NoError(t, p.f.store.AcceptContractInputMutationContext(ctx, state))
			witnesses := []graph.ContractInputWitness{{State: state, Found: true}}
			next := state
			next.InputFingerprint = "new"
			require.NoError(t, p.f.store.BeginContractInputMutationContext(ctx, &state, next, nil))
			_, moved := p.f.store.CaptureContractPublicationSource(ctx, state, witnesses)
			require.ErrorIs(t, moved, store_sqlite.ErrCatalogStaleGuard)
			require.ErrorIs(t, moved, graph.ErrContractProjectionStale)
			refusal := error(store_sqlite.ErrCatalogStaleGuard)
			if stale {
				refusal = moved
			}
			p.captureErr = func(key string, _ int) error {
				if key == "r/wt@old" {
					return refusal
				}
				return nil
			}
			old := scheduleLinkedProbe(t, p, "old", &ContractCaptureSource{})
			result := p.wait(old)
			expected := ContractActorFailed
			if stale {
				expected = ContractActorInputsStale
			}
			require.Equal(t, expected, result.Result)
			require.Empty(t, p.started(), "a refused capture runs no job")
			current := scheduleLinkedProbe(t, p, "new", &ContractCaptureSource{})
			require.Equal(t, ContractActorPublished, p.wait(current).Result)
			require.Equal(t, []string{"r/wt@new"}, p.started())
		})
	}
}
