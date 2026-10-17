package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFreshnessAdmissionExpiryPreservesOwnedDeadline(t *testing.T) {
	owned, cancelOwned := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelOwned()
	require.ErrorIs(t, owned.Err(), context.DeadlineExceeded)
	parentCanceled, cancelParent := context.WithCancel(t.Context())
	cancelParent()
	for _, tc := range []struct {
		name    string
		parent  context.Context
		waitErr error
		past    bool
		reason  string
		expired bool
	}{
		{"owned_deadline_before_wall_observation", t.Context(), owned.Err(), false, freshReasonDeadlineExceeded, true},
		{"parent_interruption_takes_precedence", parentCanceled, owned.Err(), false, freshReasonInterrupted, true},
		{"coordinator_expiry_leaves_owned_context_live", t.Context(), nil, false, "", false},
		{"owned_cancel_is_not_a_deadline", t.Context(), context.Canceled, false, "", false},
		{"observed_wall_deadline", t.Context(), nil, true, freshReasonDeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deadline := time.Now().Add(time.Hour)
			if tc.past {
				deadline = time.Now().Add(-time.Hour)
			}
			reason, expired := freshnessAdmissionBoundExpiry(tc.parent, deadline, tc.waitErr)
			require.Equal(t, tc.reason, reason)
			require.Equal(t, tc.expired, expired)
		})
	}
}
