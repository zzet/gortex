package indexer

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"go.uber.org/zap"
)

func TestContractDeltaMemoryBudgetHonorsInstalledLimit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limit, want int64
		hostRAM     uint64
	}{
		{name: "unknown", limit: 0, want: 64 << 20}, {name: "unavailable", limit: -1, want: 64 << 20},
		{name: "unlimited", limit: math.MaxInt64, want: 64 << 20}, {name: "tiny explicit", limit: 1, want: 1},
		{name: "low explicit", limit: 256 << 20, want: 16 << 20}, {name: "one GiB", limit: 1 << 30, want: 64 << 20},
		{name: "four GiB", limit: 4 << 30, want: 256 << 20}, {name: "eight GiB", limit: 8 << 30, want: 512 << 20},
		{name: "large capped", limit: 32 << 30, want: 512 << 20},
		// Without an installed limit, known host RAM sizes the budget as the
		// daemon's default standing limit would.
		{name: "no limit large host", limit: 0, hostRAM: 48 << 30, want: 512 << 20},
		{name: "no limit mid host", limit: 0, hostRAM: 16 << 30, want: 256 << 20},
		{name: "unlimited mid host", limit: math.MaxInt64, hostRAM: 16 << 30, want: 256 << 20},
		{name: "unavailable mid host", limit: -1, hostRAM: 16 << 30, want: 256 << 20},
		{name: "no limit small host", limit: 0, hostRAM: 2 << 30, want: 64 << 20},
		{name: "no limit unknown host", limit: 0, hostRAM: 0, want: 64 << 20},
		// An installed limit is authoritative whatever the host has.
		{name: "explicit eight GiB large host", limit: 8 << 30, hostRAM: 48 << 30, want: 512 << 20},
		{name: "explicit eight GiB small host", limit: 8 << 30, hostRAM: 2 << 30, want: 512 << 20},
		{name: "explicit small large host", limit: 256 << 20, hostRAM: 48 << 30, want: 16 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.want, contractDeltaMemoryBudget(tc.limit, tc.hostRAM)) })
	}
}

func TestContractDeltaMemoryBudgetRetainsFiveActorsAndHonorsPinnedPressure(t *testing.T) {
	// Conservative measured charge of the full20-repository checkpoint. Exercise
	// registry policy without allocating hundreds of megabytes of synthetic data.
	const measuredCheckpoint int64 = 84243420
	l := &contractDeltaLRU{budget: contractDeltaMemoryBudget(8<<30, 0)}
	actors := make([]ContractActor, 7)
	caches := make([]*contractDeltaCache, 7)
	for i := range actors {
		checkout := ""
		if i > 0 {
			checkout = fmt.Sprintf("checkout-%d", i)
		}
		actors[i] = NewContractActor(checkout, []string{"same-complete-cohort"})
		caches[i] = &contractDeltaCache{retainedBytes: measuredCheckpoint, complete: true}
	}
	for i := 0; i < 5; i++ {
		a := l.put(actors[i], caches[i])
		require.Equal(t, "admitted", a.reason)
		require.Zero(t, a.evicted)
		require.LessOrEqual(t, a.retained, a.budget)
	}
	retained := l.bytes
	for i := 0; i < 5; i++ {
		require.Same(t, caches[i], l.borrow(actors[i]))
	}
	require.Equal(t, retained, l.bytes, "borrowing must retain every global charge")
	require.Equal(t, "admitted", l.put(actors[5], caches[5]).reason)
	require.Same(t, caches[5], l.borrow(actors[5]))
	blocked := l.put(actors[6], caches[6])
	require.Equal(t, "pinned_pressure", blocked.reason)
	require.Zero(t, blocked.evicted)
	require.LessOrEqual(t, l.bytes, l.budget)
	// Returning one owned checkpoint makes only that actor eligible for eviction;
	// outstanding borrowed actors remain pinned and charged.
	require.Equal(t, "admitted", l.put(actors[0], caches[0]).reason)
	admitted := l.put(actors[6], caches[6])
	require.Equal(t, "admitted", admitted.reason)
	require.Equal(t, 1, admitted.evicted)
	require.Nil(t, l.borrow(actors[0]))
	require.Same(t, caches[6], l.borrow(actors[6]))
	require.Len(t, l.entries, 6)
	require.LessOrEqual(t, l.bytes, l.budget)
	low := &contractDeltaLRU{budget: contractDeltaMemoryBudget(1<<30, 0)}
	require.Equal(t, "over_budget", low.put(actors[0], caches[0]).reason)
	require.Nil(t, low.borrow(actors[0]))
}

func TestContractDeltaColdLowBudgetDoesNotCaptureOptionalNames(t *testing.T) {
	f := newContractJobFixture(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})
	options := f.options
	ctx := t.Context()
	inputs, err := options.Materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	snapshot, targets, err := NewContractFollowupCapture(options)(ctx, nil, inputs, "fixture", "")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	snapshot.Work = targets[0].Work
	witnessCalls, nameCalls := 0, 0
	snapshot.CaptureProjectionWitness = func(context.Context) (*store_sqlite.PayloadInputWitness, error) { witnessCalls++; return nil, nil }
	snapshot.positiveNameCapture = func(context.Context, int64) (*contractPositiveNameProjection, error) { nameCalls++; return nil, nil }
	session, err := newContractDeltaSession(nil, snapshot, options.Config)
	require.NoError(t, err)
	session.cacheBudget = contractDeltaMemoryBudget(1, 0)
	_, payload, err := options.Store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "low-budget-job", LayerID: "cold", GenerationKind: "contract_analysis", CreatedAt: 1})
	require.NoError(t, err)
	report, err := RunContractFollowup(ctx, ContractFollowupRequest{RebuildReason: ContractFollowupUnknownInterval, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: options.Registry, Config: options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: options.Materializer.Leases, Logger: zap.NewNop(), Yield: options.Yield, PublishedEpoch: f.epoch.Load, WaitPublished: f.waitPublished, delta: session})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Zero(t, witnessCalls, "a cold job must check its small budget before acquiring optional proof")
	require.Zero(t, nameCalls, "a cold job must not allocate the optional name projection above its budget")
}

// TestContractAnalysisCoordinatorAdmitsLargeCheckpointWithoutInstalledLimit
// pins the coordinator plumbing: with no installed limit, known host RAM lets
// the global registry admit a key-moved job's full-cohort checkpoint (the
// observed 246 MB charge) that the 64 MiB fallback refused, while unknown RAM
// and an explicit limit keep their bounds.
func TestContractAnalysisCoordinatorAdmitsLargeCheckpointWithoutInstalledLimit(t *testing.T) {
	const observedCheckpoint int64 = 246388586
	f := newContractWorkerFixture(t)
	for _, tc := range []struct {
		name    string
		limit   int64
		hostRAM uint64
		reason  string
	}{
		{"no limit known host", math.MaxInt64, 48 << 30, "admitted"},
		{"no limit unknown host", math.MaxInt64, 0, "over_budget"},
		{"explicit limit large host", 1 << 30, 48 << 30, "over_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: f.store, Leases: f.leases, Registry: f.registry, Config: f.cfg,
				MemoryLimitBytes: tc.limit, HostMemoryBytes: tc.hostRAM, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() },
				Capture: func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
					return ContractFollowupSnapshot{}, nil, graph.ErrContractProjectionStale
				}})
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			actor := NewContractActor("", []string{"full-cohort"})
			c.mu.Lock()
			admission := c.deltaCaches.put(actor, &contractDeltaCache{retainedBytes: observedCheckpoint, complete: true})
			c.mu.Unlock()
			require.Equal(t, tc.reason, admission.reason, "requested=%d budget=%d", admission.requested, admission.budget)
		})
	}
}
