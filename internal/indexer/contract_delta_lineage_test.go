package indexer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// Interrupt after real extraction/enrichment, with a real accepted write that
// supersedes the captured input key. The failed attempt must remain unpublishable.
func interruptContractLineageFixture(t *testing.T, f *contractJobFixture, p **contractDeltaCache, after func()) ContractFollowupReport {
	t.Helper()
	moved := false
	f.layers++
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{
		layer: f.layers, cache: p, epoch: f.epoch.Load, wait: f.waitPublished,
		hooks: &contractConvergenceHooks{beforeFence: func(int) {
			if !moved {
				moved = true
				after()
			}
		}},
	})
	require.True(t, moved)
	require.Equal(t, viewmetrics.ContractJobSupersededKeyMoved, contractJobOutcomeOf(err))
	require.False(t, report.Published)
	require.NotNil(t, *p)
	require.False(t, (*p).complete, "warm lineage must never certify the superseded checkpoint")
	require.Positive(t, report.Files)
	return report
}

func TestContractDeltaWarmCheckpointKeepsBaselineAndCompletedCount(t *testing.T) {
	for _, completed := range []int{0, 6} {
		t.Run(fmt.Sprintf("completed_%d", completed), func(t *testing.T) {
			files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
			f := newContractJobFixture(t, files)
			var p *contractDeltaCache
			require.Equal(t, viewmetrics.ContractModeFill, runCachedContractFixture(t, f, &p).Mode)
			require.True(t, p.hasBaseline)
			require.Zero(t, p.deltas)
			routes := func(path string) string { return strings.ReplaceAll(jobRoutes, "RoutePath", `"`+path+`"`) }
			for i := 0; i < completed; i++ {
				files["routes.go"] = routes(fmt.Sprintf("/warm-%d", i))
				f.save("routes.go", files["routes.go"])
				require.Equal(t, viewmetrics.ContractModeDelta, runCachedContractFixture(t, f, &p).Mode)
			}
			require.Equal(t, completed, p.deltas)
			heldRaw := cloneContractRecords(p.units["fixture/helper.go"].raw)
			f.save("routes.go", routes("/intermediate"))
			report := interruptContractLineageFixture(t, f, &p, func() {
				files["routes.go"] = routes("/final")
				f.save("routes.go", files["routes.go"])
			})
			require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
			require.Equal(t, 1, report.Files)
			require.True(t, p.hasBaseline, "even the first interrupted warm edit has a completed baseline")
			require.Equal(t, completed, p.deltas, "an aborted attempt is not a completed delta")
			require.Equal(t, heldRaw, p.units["fixture/helper.go"].raw)

			var extracted []string
			f.layers++
			report, err := runFixtureContractJob(t, f.options, contractFixtureJob{
				layer: f.layers, cache: &p, epoch: f.epoch.Load, wait: f.waitPublished,
				hooks: &contractConvergenceHooks{afterExtract: func(path string) { extracted = append(extracted, path) }},
			})
			require.NoError(t, err)
			require.True(t, report.Published)
			require.NotNil(t, p)
			require.True(t, p.complete)
			require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
			// The failed fence already observed the final receipt. The initial
			// receipt frontier is empty; staleUnits must still detect the old
			// router unit and extract it against the current source.
			require.Empty(t, report.ChangedFiles)
			require.Equal(t, []string{"fixture/routes.go"}, extracted)
			require.Equal(t, 1, report.Files, "resuming the warm checkpoint extracts only the moved route")
			require.True(t, p.hasBaseline)
			require.Equal(t, completed+1, p.deltas, "successful resume advances rather than resets the refresh counter")
			require.Equal(t, heldRaw, p.units["fixture/helper.go"].raw)
			got := f.contracts()
			require.Contains(t, contractIDs(got), "http::GET::/final")
			require.NotContains(t, contractIDs(got), "http::GET::/intermediate")
			require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(got))
		})
	}
}

func TestContractDeltaWarmLineageCannotBypassPeriodicRefresh(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	var p *contractDeltaCache
	runCachedContractFixture(t, f, &p)
	// Put the real completed baseline at the refresh boundary without 255
	// redundant edits. An interrupted cold refresh must discard its old lineage.
	p.deltas = contractDeltaRefresh - 1
	routes := func(path string) string { return strings.ReplaceAll(jobRoutes, "RoutePath", `"`+path+`"`) }
	f.save("routes.go", routes("/refreshing"))
	report := interruptContractLineageFixture(t, f, &p, func() {
		files["routes.go"] = routes("/after-refresh")
		f.save("routes.go", files["routes.go"])
	})
	require.Equal(t, viewmetrics.ContractModeCold, report.Mode)
	require.Equal(t, "periodic_refresh", report.CacheReason)
	require.Equal(t, len(files), report.Files)
	require.False(t, p.hasBaseline, "a partial cold refresh has no completed new baseline")
	require.Zero(t, p.deltas)
	report = runCachedContractFixture(t, f, &p)
	require.Equal(t, viewmetrics.ContractModeFill, report.Mode)
	require.Equal(t, 1, report.Files)
	require.True(t, p.hasBaseline)
	require.Zero(t, p.deltas)
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
	files["routes.go"] = routes("/next-warm")
	f.save("routes.go", files["routes.go"])
	require.Equal(t, viewmetrics.ContractModeDelta, runCachedContractFixture(t, f, &p).Mode)
	require.Equal(t, 1, p.deltas)
}
