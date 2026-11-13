package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func TestViewsHealthPreservesOnlyRegisteredIdleDeferredZero(t *testing.T) {
	viewmetrics.Reset()
	t.Cleanup(viewmetrics.Reset)
	f := newFamilyFixture(t, "registered-idle-counter")
	defer f.close()
	const idle = "views_contract_linked_jobs_total{policy=idle,event=deferred}"
	health, err := f.lc.ViewsHealth(t.Context())
	require.NoError(t, err)
	require.NotContains(t, health.Counters, idle)

	viewmetrics.Add(viewmetrics.ContractLinkedJobsTotal, 0, viewmetrics.ContractLinkedIdle, viewmetrics.ContractLinkedDeferred)
	viewmetrics.Add(viewmetrics.ContractLinkedJobsTotal, 0, viewmetrics.ContractLinkedEager, viewmetrics.ContractLinkedDeferred)
	viewmetrics.Add(viewmetrics.ContractAnalysisRequeuedTotal, 0)
	health, err = f.lc.ViewsHealth(t.Context())
	require.NoError(t, err)
	require.Contains(t, health.Counters, idle)
	require.Zero(t, health.Counters[idle])
	require.NotContains(t, health.Counters, "views_contract_linked_jobs_total{policy=eager,event=deferred}")
	require.NotContains(t, health.Counters, viewmetrics.ContractAnalysisRequeuedTotal)
	require.NotContains(t, viewmetrics.Read().Flat(), idle, "generic sparse export remains unchanged")
}
