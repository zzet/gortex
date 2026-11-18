package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// The view census rides on `daemon status`, and the one part of it that is not
// a count is the list of checkouts whose build loop could not be started.
//
// Every path that starts a coordinator is a background reconciliation with no
// caller to fail, so the reason has exactly one surface: this payload. A
// controller that fills the field and a protocol that drops it on the wire are
// the same defect from the user's side — the reason still reaches nobody — so
// the wire is pinned here, at the door `gortex daemon status` actually dials.

// censusController answers status with a views census that carries one stated
// reason, and nothing else it does not need.
type censusController struct {
	*fakeController
}

func (c *censusController) Status(_ context.Context) (StatusResponse, error) {
	return StatusResponse{
		Views: &ViewsStatus{
			Families:     1,
			Checkouts:    map[string]int{"ready": 3},
			Coordinators: 1,
			CoordinatorStartFailures: []CoordinatorStartFailure{{
				CheckoutID: "chk-1",
				RootPath:   "/tmp/worktree-a",
				Reason:     "freeze the index configuration: encode dedicated base config",
				At:         1757000000,
			}},
		},
	}, nil
}

// TestStatusCarriesTheCoordinatorStartReasonsOverTheWire is the end-to-end pin:
// a real daemon, a real control connection, the decode a CLI performs.
//
// Revert-red: remove CoordinatorStartFailures from ViewsStatus (or leave it
// unexported to the JSON encoder) and the reason arrives as an empty list,
// failing the length assertion — the "three checkouts, one build loop" census
// with nothing to explain it, which is the state this closes.
func TestStatusCarriesTheCoordinatorStartReasonsOverTheWire(t *testing.T) {
	_, socket := newDaemon(t, &censusController{fakeController: &fakeController{}})

	c, err := DialTo(socket, Handshake{Mode: ModeControl, ClientName: "cli"})
	require.NoError(t, err)
	defer c.Close()

	resp, err := c.Control(ControlStatus, StatusParams{})
	require.NoError(t, err)
	require.True(t, resp.OK, resp.ErrorMsg)

	var st StatusResponse
	require.NoError(t, json.Unmarshal(resp.Result, &st))
	require.NotNil(t, st.Views, "the status response carries no views census")
	require.Len(t, st.Views.CoordinatorStartFailures, 1,
		"the census says one checkout has no build loop and does not say why")
	got := st.Views.CoordinatorStartFailures[0]
	require.Equal(t, "chk-1", got.CheckoutID)
	require.Equal(t, "/tmp/worktree-a", got.RootPath)
	require.Contains(t, got.Reason, "freeze the index configuration")
	require.Equal(t, int64(1757000000), got.At)

	// And the counts it explains are still there: the reason is read BESIDE
	// "three checkouts, one coordinator", not instead of it.
	require.Equal(t, 3, st.Views.Checkouts["ready"])
	require.Equal(t, 1, st.Views.Coordinators)
}

// TestAHealthyViewCensusOmitsTheFailureKey pins the ordinary answer. The field
// is the one identity-bearing entry in a payload whose whole contract is
// "counts and states, never identities", and it stays affordable only because
// a healthy daemon renders no key at all rather than an empty list.
func TestAHealthyViewCensusOmitsTheFailureKey(t *testing.T) {
	body, err := json.Marshal(&ViewsStatus{Families: 1, Coordinators: 1})
	require.NoError(t, err)
	require.NotContains(t, string(body), "coordinator_start_failures")
}

// TestEnrichResultsReportASupersededRunWithoutFailing pins the wire half of the
// enrichment settlement contract: a superseded run is reported as work that
// landed, on a successful result, and the field is absent in the ordinary case.
func TestEnrichResultsReportASupersededRunWithoutFailing(t *testing.T) {
	for name, payload := range map[string]any{
		"blame":    EnrichBlameResult{Nodes: 12, Superseded: true},
		"churn":    EnrichChurnResult{Files: 3, Superseded: true},
		"releases": EnrichReleasesResult{Files: 4, Superseded: true},
		"coverage": EnrichCoverageResult{Symbols: 5, Superseded: true},
		"cochange": EnrichCochangeResult{Edges: 6, Superseded: true},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(payload)
			require.NoError(t, err)
			require.Contains(t, string(body), `"superseded":true`,
				"a superseded enrichment result does not say so on the wire")
		})
	}
	body, err := json.Marshal(EnrichBlameResult{Nodes: 12})
	require.NoError(t, err)
	require.NotContains(t, string(body), "superseded",
		"the ordinary enrichment result grew a key")
}
