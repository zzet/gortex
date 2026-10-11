package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

// The deferred retirement backlog reaches `daemon status` through
// viewsStatusFromHealth with every field; a census with nothing owed renders
// no retirement_backlog key.
func TestViewsStatusCarriesTheRetirementBacklog(t *testing.T) {
	health := indexer.ViewsHealth{
		Families:     1,
		Coordinators: 1,
		RetirementBacklog: &indexer.RetirementBacklog{
			Generations: 12, Retiring: 3, Parked: 2, BytesEstimate: 48 << 20,
			DebtAgeSeconds: 95.5, LastRemovedAt: 1_800_000_123,
		},
	}
	status := viewsStatusFromHealth(health)
	require.Equal(t, &daemon.RetirementBacklog{
		Generations: 12, Retiring: 3, Parked: 2, BytesEstimate: 48 << 20,
		DebtAgeSeconds: 95.5, LastRemovedAt: 1_800_000_123,
	}, status.RetirementBacklog)
	body, err := json.Marshal(status)
	require.NoError(t, err)
	for _, key := range []string{`"retirement_backlog"`, `"generations":12`, `"retiring":3`, `"parked":2`,
		`"bytes_estimate":50331648`, `"debt_age_s":95.5`, `"last_removed_at":1800000123`} {
		require.Contains(t, string(body), key)
	}

	idle := viewsStatusFromHealth(indexer.ViewsHealth{Families: 1})
	require.Nil(t, idle.RetirementBacklog)
	body, err = json.Marshal(idle)
	require.NoError(t, err)
	require.NotContains(t, string(body), "retirement_backlog")
}
