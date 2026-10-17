package indexer

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func runCachedContractFixture(t *testing.T, f *contractJobFixture, p **contractDeltaCache) ContractFollowupReport {
	t.Helper()
	f.layers++
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: f.layers, cache: p, logger: f.logger, epoch: f.epoch.Load, wait: f.waitPublished, adjust: f.adjust})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.NotNil(t, *p)
	require.True(t, (*p).complete)
	return report
}

func TestContractDeltaWorkerRouteReplayEqualsIndependentCold(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	core, logs := observer.New(zapcore.InfoLevel)
	f.logger = zap.New(core)
	var p *contractDeltaCache
	first := runCachedContractFixture(t, f, &p)
	require.Equal(t, viewmetrics.ContractModeFill, first.Mode)
	require.Equal(t, len(files), first.Files)
	previousClock := p.clock.clock
	unchanged := p.units["fixture/helper.go"].raw
	heldRaw := p.units["fixture/routes.go"].raw
	beforeRaw := cloneContractRecords(heldRaw)
	for i, path := range []string{"/second", "/third"} {
		files["routes.go"] = strings.ReplaceAll(jobRoutes, "RoutePath", `"`+path+`"`)
		f.save("routes.go", files["routes.go"])
		report := runCachedContractFixture(t, f, &p)
		require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
		require.Equal(t, ContractFollowupChangedBoundary, report.RebuildReason)
		require.Equal(t, []string{"fixture/routes.go"}, report.ChangedFiles)
		require.Equal(t, 1, report.Files)
		require.Greater(t, p.clock.clock, previousClock)
		require.Equal(t, unchanged, p.units["fixture/helper.go"].raw)
		require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
		require.Equal(t, beforeRaw, heldRaw)
		require.Equal(t, i+1, p.deltas)
		checkpoints := logs.FilterMessage("contract cache checkpoint").All()
		require.EqualValues(t, 1, checkpoints[len(checkpoints)-1].ContextMap()["normalized_raw_units"], "warm work only normalizes newly extracted raw")
		require.LessOrEqual(t, p.retainedBytes, contractDeltaCacheBudget)
		previousClock = p.clock.clock
	}
	// A postpass must never mutate the original raw unit across later jobs.
	require.NotEmpty(t, beforeRaw)
	require.Equal(t, beforeRaw, heldRaw)
}

func TestContractDeltaWorkerProducerLifecycleEqualsIndependentCold(t *testing.T) {
	for _, row := range []struct {
		name, initial, edited string
		remove                bool
	}{
		{name: "value", initial: jobConsts, edited: strings.ReplaceAll(jobConsts, "/before", "/after")},
		{name: "rename", initial: jobConsts, edited: strings.ReplaceAll(jobConsts, "RoutePath", "RenamedPath")},
		{name: "negative_becomes_answered", initial: "package fixture\nconst OtherPath = \"/other\"\n", edited: jobConsts},
		{name: "remove", initial: jobConsts, remove: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			files := map[string]string{"routes.go": jobRoutes, "consts.go": row.initial, "helper.go": jobHelper}
			f := newContractJobFixture(t, files)
			var p *contractDeltaCache
			runCachedContractFixture(t, f, &p)
			oldClock := p.clock.clock
			if row.remove {
				f.remove("consts.go")
				delete(files, "consts.go")
			} else {
				f.save("consts.go", row.edited)
				files["consts.go"] = row.edited
			}
			report := runCachedContractFixture(t, f, &p)
			require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
			require.Greater(t, p.clock.clock, oldClock)
			require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
			require.Less(t, report.Files, len(files)+1)
			if !row.remove {
				require.Contains(t, report.ChangedFiles, "fixture/consts.go")
			}
		})
	}
}

func TestContractFillCheckpointResumesMissingUnitsAndEqualsCold(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts}
	for i := 0; i < 33; i++ {
		files[fmt.Sprintf("helper%02d.go", i)] = fmt.Sprintf("package fixture\nfunc Helper%d() int { return %d }\n", i, i)
	}
	f := newContractJobFixture(t, files)
	var p *contractDeltaCache
	scratch := t.TempDir()
	beforeHeld := f.options.Materializer.Leases.Held()
	beforeWork, err := f.options.Store.ContractWorkForScopeContext(t.Context(), "fixture", "")
	require.NoError(t, err)
	require.NotEmpty(t, beforeWork)
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: 1, cache: &p, scratchParent: scratch, otherActorWaiting: func() bool { return true }})
	require.ErrorAs(t, err, new(*ContractFillContinued))
	require.Equal(t, viewmetrics.ContractJobFillContinued, contractJobOutcomeOf(err))
	require.False(t, report.Published)
	require.Zero(t, report.EnrichRuns, "a partial fill never enters enrichment")
	require.NotNil(t, p)
	require.False(t, p.complete)
	require.Len(t, p.units, 16)
	require.Equal(t, beforeHeld, f.options.Materializer.Leases.Held(), "snapshot and payload leases return before continuation")
	entries, err := os.ReadDir(scratch)
	require.NoError(t, err)
	require.Empty(t, entries, "round scratch closes before continuation")
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	attachment, err := f.options.Store.GetContractAttachmentContext(t.Context(), graph.ContractAttachmentKey{RepoPrefix: "fixture", InputVersion: inputs.State.InputVersion, InputFingerprint: inputs.State.InputFingerprint})
	require.NoError(t, err)
	require.Nil(t, attachment, "a partial fill never publishes an attachment")
	afterWork, err := f.options.Store.ContractWorkForScopeContext(t.Context(), "fixture", "")
	require.NoError(t, err)
	require.Equal(t, beforeWork, afterWork, "partial fill leaves captured work unacknowledged")
	kept := make(map[string]*contractUnit)
	for path, unit := range p.units {
		kept[path] = unit
	}
	files["helper00.go"] = "package fixture\nfunc Helper0() int { return 999 }\nfunc Added() {}\n"
	f.save("helper00.go", files["helper00.go"])
	report, err = runFixtureContractJob(t, f.options, contractFixtureJob{layer: 2, cache: &p})
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, viewmetrics.ContractModeFill, report.Mode)
	require.Equal(t, len(files)-len(kept)+1, report.Files, "only missing and stale units extract on resume")
	require.True(t, p.complete)
	for path, unit := range kept {
		if path != "fixture/helper00.go" {
			require.Equal(t, unit.raw, p.units[path].raw)
		}
	}
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
}

func TestContractFillIdleCallbackPreservesDefaultRoundSize(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts}
	for i := 0; i < 70; i++ {
		files[fmt.Sprintf("helper%02d.go", i)] = fmt.Sprintf("package fixture\nfunc Helper%d() int { return %d }\n", i, i)
	}
	var rounds int
	for _, waiting := range []func() bool{nil, func() bool { return false }} {
		f := newContractJobFixture(t, files)
		var p *contractDeltaCache
		report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: 1, cache: &p, otherActorWaiting: waiting})
		require.NoError(t, err)
		require.True(t, report.Published)
		if waiting == nil {
			rounds = report.Rounds
		} else {
			require.Equal(t, rounds, report.Rounds, "an idle scheduler callback must not split normal fill rounds")
		}
	}
	f := newContractJobFixture(t, files)
	var p *contractDeltaCache
	report, err := runFixtureContractJob(t, f.options, contractFixtureJob{layer: 1, cache: &p, otherActorWaiting: func() bool { return true }})
	require.ErrorAs(t, err, new(*ContractFillContinued))
	require.Equal(t, 16, report.Files, "contention limits the next admitted extraction round")
}

func TestContractDeltaUnitRetainsItsMeasuredRawSize(t *testing.T) {
	f := newContractJobFixture(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper})
	core, logs := observer.New(zapcore.InfoLevel)
	f.logger = zap.New(core)
	var cache *contractDeltaCache
	runCachedContractFixture(t, f, &cache)
	sizes := map[string]int64{}
	for path, unit := range cache.units {
		require.Positive(t, unit.retainedRawBytes, "even an empty raw slice has owned header storage")
		require.Equal(t, contractRetainedSize(reflect.ValueOf(unit.raw))+contractRetainedSize(reflect.ValueOf(unit.router)), unit.retainedRawBytes)
		sizes[path] = unit.retainedRawBytes
	}
	f.save("routes.go", strings.ReplaceAll(jobRoutes, "RoutePath", `"/after"`))
	runCachedContractFixture(t, f, &cache)
	for path, unit := range cache.units {
		require.Equal(t, contractRetainedSize(reflect.ValueOf(unit.raw))+contractRetainedSize(reflect.ValueOf(unit.router)), unit.retainedRawBytes)
		if path != "fixture/routes.go" {
			require.Equal(t, sizes[path], unit.retainedRawBytes)
		}
	}
	checkpoints := logs.FilterMessage("contract cache checkpoint").All()
	require.EqualValues(t, 1, checkpoints[len(checkpoints)-1].ContextMap()["normalized_raw_units"], "unchanged raw values retain their measurement across ownership transfer")
	require.LessOrEqual(t, cache.retainedBytes, contractDeltaCacheBudget)
}
