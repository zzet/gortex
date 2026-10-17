//go:build performance

package indexer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestContractStuckRepairTakesAReleasedLaneWithinItsBudget: a repair that
// found the lane busy takes it within 200 ms of its release; the test
// allows twice that.
func TestContractStuckRepairTakesAReleasedLaneWithinItsBudget(t *testing.T) {
	busyLaneRepair(t, func(d time.Duration) { require.Less(t, d, time.Second) }, func(d time.Duration) { require.LessOrEqual(t, d, 2*200*time.Millisecond) })
}

func TestContractStuckReceiptRepairMeetsItsPeriodBudget(t *testing.T) {
	repairStuckReceipt(t, func(d time.Duration) {
		require.Less(t, d, contractRepairOrphanGrace+contractRepairTestPeriod+2*time.Second)
	})
}

func TestContractStuckRepairMeetsItsBudgetUnderSteadyEvents(t *testing.T) {
	repairUnderSteadyEvents(t, func(d time.Duration) {
		require.Less(t, d, contractRepairOrphanGrace+contractRepairTestPeriod+3*time.Second)
	})
}

func TestContractStuckRepairMeetsItsWindowAgeBudget(t *testing.T) {
	stuckRepairMeasured(t, func(f *contractRepairFixture) { f.stageInFlight("fixture/consts.go") }, func(d time.Duration) { require.Less(t, d, 3*contractRepairTestPeriod+2*time.Second) })
}
