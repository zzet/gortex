package mcp

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

func countContractRegistryLoads(srv *Server) *atomic.Int64 {
	var loads atomic.Int64
	srv.contractRegistries.load = func(ctx context.Context, r graph.Reader, opts contracts.RegistryLoadOptions) (*contracts.Registry, error) {
		loads.Add(1)
		return contracts.LoadRegistryFromGraphChecked(ctx, r, opts)
	}
	return &loads
}

type contractImpactRun struct {
	impact   *contractImpact
	records  []contracts.Contract
	readErr  error
	validErr error
}

// runOptionalContractImpact answers one impact request on its own binding, as
// a separate RPC would. uncached detaches the binding from the server cache.
func runOptionalContractImpact(t *testing.T, f *contractConsumerFixture, uncached bool, changed ...string) contractImpactRun {
	t.Helper()
	ctx := prepareOptionalForTest(t, t.Context(), f, false)
	binding := contractAnalysisFromContext(ctx)
	defer binding.close()
	if uncached {
		binding.registries = nil
	}
	run := contractImpactRun{impact: f.srv.computeContractImpactContext(ctx, changed)}
	if reg := f.srv.optionalContractRegistryForContext(ctx); reg != nil {
		run.records = reg.All()
		sort.Slice(run.records, func(i, j int) bool {
			return fmt.Sprint(run.records[i].ID, run.records[i].RepoPrefix, run.records[i].Role) < fmt.Sprint(run.records[j].ID, run.records[j].RepoPrefix, run.records[j].Role)
		})
	}
	if run.impact != nil {
		sort.Slice(run.impact.Affected, func(i, j int) bool {
			return fmt.Sprint(run.impact.Affected[i]) < fmt.Sprint(run.impact.Affected[j])
		})
	}
	run.readErr = binding.readError()
	run.validErr = binding.validate(ctx)
	return run
}

func TestOptionalContractImpactReusesUnchangedRegistryAcrossRequests(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/current")
	fake := installForbiddenContractRuntime(t, f.srv)
	loads := countContractRegistryLoads(f.srv)
	changed := []string{"repo/types.go::Response", "b/types.go::Response"}

	first := runOptionalContractImpact(t, f, false, changed...)
	require.NoError(t, first.validErr)
	require.NotNil(t, first.impact)
	require.Len(t, first.impact.Affected, 2)
	require.Equal(t, int64(2), loads.Load(), "one checked load per admitted repository")

	for range 3 {
		again := runOptionalContractImpact(t, f, false, changed...)
		require.NoError(t, again.validErr)
		require.Equal(t, first, again)
	}
	require.Equal(t, int64(2), loads.Load(), "unchanged inputs reuse the loaded registries")

	uncached := runOptionalContractImpact(t, f, true, changed...)
	require.NoError(t, uncached.validErr)
	require.Equal(t, uncached, first, "a reused registry answers exactly as a fresh load")
	require.Equal(t, int64(2), loads.Load())
	require.Zero(t, fake.schedules.Load())
	require.Zero(t, fake.waits.Load())
}

func TestOptionalContractImpactReloadsAfterCoreSourceMutation(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/current")
	installForbiddenContractRuntime(t, f.srv)
	loads := countContractRegistryLoads(f.srv)
	changed := []string{"repo/types.go::Response"}

	first := runOptionalContractImpact(t, f, false, changed...)
	require.NoError(t, first.validErr)
	require.NotNil(t, first.impact)
	require.Equal(t, int64(2), loads.Load())

	// An unrelated core write moves the clocks without changing the answer.
	f.store.AddBatch([]*graph.Node{{ID: "repo/other.go::Other", Name: "Other", Kind: graph.KindFunction, FilePath: "repo/other.go", RepoPrefix: "repo"}}, nil)
	second := runOptionalContractImpact(t, f, false, changed...)
	require.NoError(t, second.validErr)
	require.Equal(t, first, second)
	require.Equal(t, int64(4), loads.Load(), "a core write invalidates every repository's registry")

	// Removing an owner's current declaration leaves the attachment and its
	// inputs unchanged, but the checked projection now refuses. A reused
	// registry would hide that refusal.
	f.store.EvictFile("repo/handler.go")
	removed := runOptionalContractImpact(t, f, false, changed...)
	require.Nil(t, removed.impact)
	require.Nil(t, removed.records)
	require.ErrorIs(t, removed.readErr, graph.ErrContractProjectionIncomplete)
	require.ErrorIs(t, removed.validErr, graph.ErrContractProjectionIncomplete)
	require.Equal(t, int64(6), loads.Load())
	uncached := runOptionalContractImpact(t, f, true, changed...)
	require.Equal(t, uncached, removed)
}

func TestOptionalContractImpactReloadsForNewAttachment(t *testing.T) {
	f := newContractConsumerFixture(t)
	f.publish(t, nil, "", "/current")
	installForbiddenContractRuntime(t, f.srv)
	loads := countContractRegistryLoads(f.srv)
	changed := []string{"repo/types.go::Response"}

	first := runOptionalContractImpact(t, f, false, changed...)
	require.NoError(t, first.validErr)
	require.NotNil(t, first.impact)
	require.Equal(t, "http::GET::/current", first.impact.Affected[0].ContractID)
	require.Equal(t, int64(2), loads.Load())

	moveConsumerInput(t, f, "next")
	f.publish(t, nil, "", "/next")
	next := runOptionalContractImpact(t, f, false, changed...)
	require.NoError(t, next.validErr)
	require.NotNil(t, next.impact)
	require.Equal(t, "http::GET::/next", next.impact.Affected[0].ContractID)
	for _, record := range next.records {
		require.NotEqual(t, "http::GET::/current", record.ID)
	}
	require.Equal(t, int64(4), loads.Load(), "a new attachment loads its own registry")

	again := runOptionalContractImpact(t, f, false, changed...)
	require.Equal(t, next, again)
	require.Equal(t, int64(4), loads.Load())
	require.Equal(t, runOptionalContractImpact(t, f, true, changed...), next)
}

func TestContractRegistryCacheBoundsSlotsMostRecentlyUsedFirst(t *testing.T) {
	var cache contractRegistryCache
	var identity graphview.ContractRegistryIdentity
	key := func(i int) contractRegistryCacheKey { return contractRegistryCacheKey{repo: fmt.Sprintf("repo-%d", i)} }
	registries := make([]*contracts.Registry, contractRegistryCacheSlots+1)
	for i := range registries {
		registries[i] = contracts.NewRegistry()
		cache.store(key(i), identity, registries[i])
		if i == contractRegistryCacheSlots-1 {
			// Reading the oldest slot keeps it past the next store.
			got, hit := cache.lookup(key(0), identity)
			require.True(t, hit)
			require.Same(t, registries[0], got)
		}
	}
	require.Len(t, cache.entries, contractRegistryCacheSlots)
	_, hit := cache.lookup(key(1), identity)
	require.False(t, hit, "the least recently used slot is dropped")
	got, hit := cache.lookup(key(0), identity)
	require.True(t, hit)
	require.Same(t, registries[0], got)

	// A namespace keeps only its latest load.
	replacement := contracts.NewRegistry()
	cache.store(key(0), identity, replacement)
	require.Len(t, cache.entries, contractRegistryCacheSlots)
	got, hit = cache.lookup(key(0), identity)
	require.True(t, hit)
	require.Same(t, replacement, got)
}
