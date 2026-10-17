package indexer

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestContractDeltaAdmissionReportsBoundAndPinnedPressure(t *testing.T) {
	a, b, c := NewContractActor("", []string{"a"}), NewContractActor("", []string{"b"}), NewContractActor("", []string{"c"})
	p := &contractDeltaCache{}
	l := &contractDeltaLRU{}
	admitted := l.put(a, p)
	require.Equal(t, "admitted", admitted.reason)
	require.Equal(t, contractDeltaCacheBudget, admitted.budget)
	require.Equal(t, l.bytes, admitted.retained)
	l.budget = admitted.requested - 1
	rejected := l.put(a, p)
	require.Equal(t, "over_budget", rejected.reason)
	require.Greater(t, rejected.requested, rejected.budget)
	require.Zero(t, rejected.retained)
	require.Nil(t, l.borrow(a))
	l.budget = 2 * admitted.requested
	require.Equal(t, "admitted", l.put(a, p).reason)
	require.Equal(t, "admitted", l.put(b, p).reason)
	require.Same(t, p, l.borrow(a))
	require.Same(t, p, l.borrow(b))
	blocked := l.put(c, p)
	require.Equal(t, "pinned_pressure", blocked.reason)
	require.LessOrEqual(t, blocked.retained, blocked.budget)
	require.Equal(t, "discarded", l.put(a, nil).reason)
	require.Equal(t, "admitted", l.put(c, p).reason)
}

func TestContractDeltaSessionReportsRejectedMetadataWithoutValues(t *testing.T) {
	for _, row := range []struct {
		name         string
		value        any
		reason, kind string
	}{
		{name: "struct", value: struct{ Secret string }{Secret: "private-value"}, reason: "unsupported_metadata", kind: "struct { Secret string }"},
		{name: "function", value: func() {}, reason: "invalid_metadata_json", kind: "func()"},
	} {
		t.Run(row.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			session := &contractDeltaSession{knownRaw: make(map[*contractUnit]int64)}
			unit := &contractUnit{path: "repo/routes.go", raw: []contracts.Contract{{Meta: map[string]any{"payload": row.value}}}}
			session.ownRaw(unit)
			session.finish(&contractConvergence{logger: zap.New(core)}, true)
			require.Nil(t, session.cache)
			entries := logs.FilterMessage("contract cache checkpoint").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			require.Equal(t, row.reason, fields["reason"])
			require.Equal(t, row.kind, fields["metadata_type"])
			require.Equal(t, "repo/routes.go", fields["file"])
			require.NotContains(t, fields, "metadata_value")
			if row.name == "struct" {
				require.Equal(t, "meta.payload", fields["metadata_path"])
			}
		})
	}
}

func TestContractDeltaSessionReportsPreparedCheckpointCharge(t *testing.T) {
	f := newRouteConstantFixture(t)
	core, logs := observer.New(zapcore.InfoLevel)
	f.logger = zap.New(core)
	var p *contractDeltaCache
	runCachedContractFixture(t, f, &p)
	entries := logs.FilterMessage("contract cache checkpoint").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "prepared", fields["reason"])
	require.Equal(t, true, fields["complete"])
	require.Equal(t, p.retainedBytes, fields["retained_bytes"])
	require.Equal(t, p.retainedBytes, fields["raw_bytes"].(int64)+fields["proof_bytes"].(int64))
	plan := logs.FilterMessage("contract delta plan").All()
	require.Len(t, plan, 1)
	require.EqualValues(t, len(p.live), plan[0].ContextMap()["extract_count"])
	require.Len(t, plan[0].ContextMap()["extract_paths"], min(16, len(p.live)))
	admission := (&contractDeltaLRU{}).put(NewContractActor("", []string{"fixture"}), p)
	require.Equal(t, "admitted", admission.reason)
	require.GreaterOrEqual(t, admission.requested, p.retainedBytes)
}

func TestContractDeltaBorrowLogPanicReturnsCacheCustody(t *testing.T) {
	var panicked atomic.Bool
	core, _ := observer.New(zapcore.InfoLevel)
	p := newSchedulerProbe(t, func(options *ContractAnalysisCoordinatorOptions) {
		options.Logger = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
			if entry.Message == "contract cache borrow" && panicked.CompareAndSwap(false, true) {
				panic("borrow log failure")
			}
			return nil
		}))
	})
	actor := NewContractActor("wt", []string{"r"})
	p.c.mu.Lock()
	p.c.deltaCaches.put(actor, &contractDeltaCache{})
	p.c.mu.Unlock()
	recoverPanicPin(t, p, "wt", &ContractCaptureSource{}, func() {
		p.c.mu.Lock()
		defer p.c.mu.Unlock()
		require.Zero(t, p.c.deltaCaches.bytes, "a failed borrow log releases its pinned charge")
		require.Empty(t, p.c.deltaCaches.entries)
	})
	require.True(t, panicked.Load())
}
