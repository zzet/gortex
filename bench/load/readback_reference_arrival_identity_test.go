package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func readbackArrivalWireIdentity(t *testing.T, arrival []editProbeArrival, include bool) readbackReferenceComparisonIdentity {
	t.Helper()
	wire := map[string]any{
		"binary_sha256": "b", "manifest_sha256": "m", "host_sha256": "h",
		"population_sha256": "p", "read_plan_sha256": "r", "agents": 8,
		"kind": "modify_body", "tool_surface": readbackReferenceCompatibilitySurface,
	}
	if include {
		hash, err := hashJSON(arrival)
		require.NoError(t, err)
		wire["arrival_sha256"] = hash
	}
	raw, err := json.Marshal(wire)
	require.NoError(t, err)
	var identity readbackReferenceComparisonIdentity
	require.NoError(t, json.Unmarshal(raw, &identity))
	return identity
}

func TestReadbackReferenceBaselineCannotBorrowAnotherArrivalStream(t *testing.T) {
	arrival, err := editProbeArrivals(1, 100, 8, 15*time.Second)
	require.NoError(t, err)
	under := readbackArrivalWireIdentity(t, arrival, true)
	p95, baseline := Distribution{P95: measured(100, "ms")}, Distribution{P95: measured(80, "ms")}
	require.NoError(t, compareReadbackReferenceTiming("editing_context", 8, p95, baseline, under, under))
	for _, mutation := range []string{"due", "editor", "missing"} {
		t.Run(mutation, func(t *testing.T) {
			changed := append([]editProbeArrival(nil), arrival...)
			switch mutation {
			case "due":
				changed[7].DueNS++
			case "editor":
				changed[7].Editor = (changed[7].Editor + 1) % 8
			}
			foreign := readbackArrivalWireIdentity(t, changed, mutation != "missing")
			require.Error(t, compareReadbackReferenceTiming("editing_context", 8, p95, baseline, under, foreign), "same source/host/fixture/read requests cannot prove a different or unknown offered stream")
		})
	}
}
