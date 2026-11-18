//go:build acceptance

package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadbackReferenceHostMismatchRetainsBothSnapshots(t *testing.T) {
	before := readbackReferenceHostSnapshot{Host: Host{Profile: "native", Limits: map[string]Metric{"disk_capacity_bytes": measured(4e12, "bytes")}}, ProbePath: "/existing/parent", VolumeIdentity: "device=1;fsid=one"}
	for name, after := range map[string]readbackReferenceHostSnapshot{
		"same":             before,
		"different_volume": {Host: before.Host, ProbePath: before.ProbePath, VolumeIdentity: "device=2;fsid=two"},
		"missing_snapshot": {ProbePath: before.ProbePath, Error: "injected statfs unavailable"},
		"capacity_changed": {Host: Host{Profile: "native", Limits: map[string]Metric{"disk_capacity_bytes": measured(1e12, "bytes")}}, ProbePath: before.ProbePath, VolumeIdentity: before.VolumeIdentity},
	} {
		t.Run(name, func(t *testing.T) {
			report := readbackReferenceCaseReport{HostBefore: before}
			err := report.retainHostAfter(after)
			if name == "same" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			raw, marshalErr := json.Marshal(report)
			require.NoError(t, marshalErr)
			var saved readbackReferenceCaseReport
			require.NoError(t, json.Unmarshal(raw, &saved))
			require.Equal(t, before, saved.HostBefore)
			require.Equal(t, after, saved.HostAfter)
		})
	}
}

func TestReadbackReferenceDiagnosticCountFieldsAreSeparate(t *testing.T) {
	report := readbackReferenceCaseReport{ProtocolOnly: true, ExpectedOffers: 1, ObservedOffers: 1, AcceptanceNominalOffers: 100, Cell: readbackReferenceCell{Offers: 100}, Correctness: summarizeReadbackReference(1, []bool{false})}
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	var saved map[string]any
	require.NoError(t, json.Unmarshal(raw, &saved))
	require.Equal(t, float64(1), saved["expected_offers"])
	require.Equal(t, float64(1), saved["observed_offers"])
	require.Equal(t, float64(100), saved["acceptance_nominal_offers"])
	require.Equal(t, float64(0), saved["correctness"].(map[string]any)["missing"])
}
