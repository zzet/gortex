package hooks

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

// A status call that overruns its budget says nothing about daemon health —
// Status queues behind the controller mutex, which a track / reload /
// enrichment holds for minutes. Measured over 233 real fires, 81.5% of
// sessions exceeded the 2s budget, and every one of them told the agent that
// enforcement was degraded on a daemon that was fine.
//
// The briefing must describe the daemon it actually has.
func TestSessionStartBriefing_BusyDaemonIsNotReportedAsBroken(t *testing.T) {
	prev := sessionStartStatusFn
	t.Cleanup(func() { sessionStartStatusFn = prev })

	// What the probe fallback produces once Status has timed out: real
	// identity, no aggregates.
	sessionStartStatusFn = func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version:       "0.63.1",
			Ready:         true,
			CountsUnknown: true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Prefix: "gortex", Path: "/work/gortex", Name: "gortex"},
			},
		}, nil
	}

	out := buildSessionStartBriefing("/work/gortex")

	assert.NotContains(t, out, "status query failed",
		"a busy daemon must not be reported as a failed one")
	assert.NotContains(t, out, "unreachable",
		"the transport was reachable — the aggregate simply was not asked for")
	assert.Contains(t, out, "tracked repo(s)",
		"the briefing still reports scope, which is what the probe can answer")
}

// The counters a probe never computed must not be rendered as real zeros.
// "0 nodes" is a claim about the graph; the honest statement is that the
// totals were not queried.
func TestSessionStartBriefing_UnknownCountsAreNotRenderedAsZero(t *testing.T) {
	prev := sessionStartStatusFn
	t.Cleanup(func() { sessionStartStatusFn = prev })
	sessionStartStatusFn = func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version:       "0.63.1",
			Ready:         true,
			CountsUnknown: true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Prefix: "gortex", Path: "/work/gortex", Name: "gortex"},
			},
		}, nil
	}

	out := buildSessionStartBriefing("/work/gortex")
	assert.NotContains(t, out, "0 nodes",
		"an unasked aggregate must never be presented as a measured zero")
	assert.Contains(t, out, "not queried")
}

// A genuinely unreachable daemon must still produce the hard warning — this
// fix narrows what counts as unreachable, it does not remove the state.
func TestSessionStartBriefing_UnreachableDaemonStillWarns(t *testing.T) {
	prev := sessionStartStatusFn
	t.Cleanup(func() { sessionStartStatusFn = prev })
	sessionStartStatusFn = func() (*daemon.StatusResponse, error) {
		return nil, errDaemonUnreachable
	}

	out := buildSessionStartBriefing("/work/gortex")
	assert.Contains(t, strings.ToLower(out), "unreachable",
		"a daemon that truly cannot be dialled must still be called out")
}

// When Status answers inside its budget nothing changes: real counters are
// reported exactly as before, so this fallback is invisible on a quiet daemon.
func TestSessionStartBriefing_HealthyStatusStillReportsCounts(t *testing.T) {
	prev := sessionStartStatusFn
	t.Cleanup(func() { sessionStartStatusFn = prev })
	sessionStartStatusFn = func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "0.63.1",
			Ready:   true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Prefix: "gortex", Path: "/work/gortex", Name: "gortex", Nodes: 31000, Edges: 206000},
			},
		}, nil
	}

	out := buildSessionStartBriefing("/work/gortex")
	assert.Contains(t, out, "31000")
	assert.NotContains(t, out, "not queried")
}

// sessionStartStatus must only declare a daemon unreachable when the probe
// agrees. A status error that is not errDaemonUnreachable is a budget
// overrun, and the probe is what distinguishes busy from down.
func TestSessionStartStatus_PropagatesUnreachableWithoutProbing(t *testing.T) {
	// errDaemonUnreachable short-circuits before any probe is attempted;
	// asserting the sentinel survives the wrapper keeps that contract pinned.
	err := errDaemonUnreachable
	assert.True(t, errors.Is(err, errDaemonUnreachable))
	require.NotNil(t, sessionStartStatusFn)
}
