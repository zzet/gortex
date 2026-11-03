package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func linuxReadbackFrozenPlan() readbackReferencePlan {
	return readbackReferencePlan{Version: 1, Seed: 1, EditMeanNS: int64(15 * time.Second), ProductDeadlineSeconds: 120, WarmSeconds: 1800, StopSeconds: 30, ToolSurface: readbackReferenceCompatibilitySurface}
}

func TestLinuxReadbackWholeCellBudgetCoversFrozen100Offers(t *testing.T) {
	p := linuxReadbackFrozenPlan()
	schedules, err := readbackReferenceSchedules(p)
	require.NoError(t, err)
	require.Len(t, schedules, 90)
	for _, index := range []int{0, 18, 36, 54, 72} {
		schedule := schedules[index]
		t.Run(fmt.Sprint(schedule.Cell.Agents), func(t *testing.T) {
			require.Len(t, schedule.Arrival, 100)
			phase := (time.Duration(schedule.PhaseBoundNS) + time.Second - 1) / time.Second
			minimum := p.WarmSeconds + int(phase) + p.StopSeconds + 30 + 60
			// Derive a full-cell cap rather than reusing the2160-second diagnostic bound.
			catalog, err := readbackReferenceLinuxLaunchCatalog(p)
			require.NoError(t, err)
			actualWorkCap := catalog.Cells[index].WorkCapSeconds
			require.GreaterOrEqual(t, actualWorkCap, minimum, "whole-cell cap must cover setup, unchanged offered tail/drain, stop and existing report/closure margins")
		})
	}
}

func TestLinuxReadbackCatalogKeepsCountsRolesAndMargins(t *testing.T) {
	p := linuxReadbackFrozenPlan()
	catalog, err := readbackReferenceLinuxLaunchCatalog(p)
	require.NoError(t, err)
	require.Len(t, catalog.Cells, 90)
	expectedWork := []int{3669, 2541, 2353, 2242, 2208}
	for group, index := range []int{0, 18, 36, 54, 72} {
		c := catalog.Cells[index]
		require.Equal(t, expectedWork[group], c.WorkCapSeconds)
		require.Equal(t, c.ClientTimeoutSeconds+60, c.WorkCapSeconds)
		require.Equal(t, c.WorkCapSeconds+120, c.HostTotalCapSeconds)
	}
	for index, c := range catalog.Cells {
		require.Equal(t, index, c.Index)
		require.Equal(t, 100, c.ExpectedOffers)
		require.Len(t, c.Schedule.Arrival, 100)
		require.Equal(t, 100, c.Schedule.Cell.Offers)
		planned, err := hashJSON(c.Schedule.Arrival)
		require.NoError(t, err)
		require.Equal(t, planned, c.PlannedArrivalSHA256)
		require.Equal(t, c.Schedule.Arrival[99].DueNS, c.Schedule.OfferedTailNS)
		require.Equal(t, c.Schedule.OfferedTailNS+int64(240*time.Second), c.Schedule.PhaseBoundNS)
	}
	require.Equal(t, "UNGRANTED_METADATA_ONLY_NO_RUNTIME", catalog.State)
}

func TestLinuxReadbackCatalogRefusesScheduleScopeChanges(t *testing.T) {
	for _, change := range []func(*readbackReferencePlan){
		func(p *readbackReferencePlan) { p.Language = "typescript" },
		func(p *readbackReferencePlan) { p.Seed = 2 },
		func(p *readbackReferencePlan) { p.EditMeanNS = int64(time.Second) },
		func(p *readbackReferencePlan) { p.ProductDeadlineSeconds = 121 },
		func(p *readbackReferencePlan) { p.WarmSeconds = 1 },
		func(p *readbackReferencePlan) { p.StopSeconds = 31 },
		func(p *readbackReferencePlan) { p.ToolSurface = readbackReferenceFacadeSurface },
	} {
		p := linuxReadbackFrozenPlan()
		change(&p)
		_, err := readbackReferenceLinuxLaunchCatalog(p)
		require.Error(t, err)
	}
}
