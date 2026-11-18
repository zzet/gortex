//go:build performance

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func TestDaemonContractReceiptRepairMeetsItsPeriodBudget(t *testing.T) {
	daemonContractPublicationAndRepair(t, func(d time.Duration) { require.Less(t, d, 3*indexer.ContractRepairPeriod) })
}
