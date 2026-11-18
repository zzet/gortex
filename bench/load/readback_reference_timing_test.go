package main

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestReadbackReferenceLatencyKeepsMissingRanksAndMeasuredZero(t *testing.T) {
	rows := make([]readbackReferenceRecord, 100)
	for i := range rows {
		rows[i].Probes = map[string]readbackReferenceRPC{"source": {Correct: true, Latency: measured(0, "ms")}}
	}
	d := readbackReferenceTiming(rows, "source", 100)
	require.NotNil(t, d.P95.Value)
	require.Zero(t, *d.P95.Value)
	for i := 94; i < 100; i++ {
		rows[i].Probes["source"] = readbackReferenceRPC{Latency: unavailable("ms", "censored")}
	}
	d = readbackReferenceTiming(rows, "source", 100)
	require.Nil(t, d.P95.Value)
	require.Equal(t, 94, d.Count)
}
func TestReadbackReferenceBaselineReuseRequiresEveryActualIdentity(t *testing.T) {
	id := readbackReferenceComparisonIdentity{BinarySHA256: "b", ManifestSHA256: "m", HostSHA256: "h", PopulationSHA256: "p", ReadPlanSHA256: "r", ArrivalSHA256: "a", Agents: 8, Kind: "modify_body", ToolSurface: readbackReferenceFacadeSurface}
	under := Distribution{P95: measured(100, "ms")}
	baseline := Distribution{P95: measured(80, "ms")}
	require.NoError(t, compareReadbackReferenceTiming("editing_context", 8, under, baseline, id, id))
	foreign := id
	foreign.HostSHA256 = "other"
	require.Error(t, compareReadbackReferenceTiming("editing_context", 8, under, baseline, id, foreign))
	under.P95 = measured(106, "ms")
	require.Error(t, compareReadbackReferenceTiming("editing_context", 8, under, baseline, id, id), "25ms delta remains binding even below absolute140ms")
	id.Agents = 4
	require.Error(t, compareReadbackReferenceTiming("source", 4, Distribution{P95: measured(40, "ms"), P99: unavailable("ms", "one censored rank")}, Distribution{P95: measured(30, "ms")}, id, id))
}

func TestReadbackReferenceMainComparisonUsesAllLogicalOffersAndFivePercent(t *testing.T) {
	phase := Phase{Attempts: []Attempt{{Role: "workload", Tool: "read.source", Agent: 0, Step: 0, Request: "r", Retry: 0, Outcome: "busy", LogicalMS: 1}, {Role: "workload", Tool: "read.source", Agent: 0, Step: 0, Request: "r", Retry: 1, Outcome: "success", LogicalMS: 60}}}
	q := readbackReferenceMainP95(phase, "read.source")
	require.NotNil(t, q.Value)
	require.Equal(t, 60.0, *q.Value, "retry wait is part of original logical time")
	phase.Attempts = append(phase.Attempts, Attempt{Role: "workload", Tool: "read.source", Agent: 0, Step: 1, Request: "r", Outcome: "phase_censored"})
	require.Nil(t, readbackReferenceMainP95(phase, "read.source").Value)
	require.Error(t, compareReadbackReferenceMain(Report{}, Report{}, "main", "candidate"), "missing main comparison cannot imply no regression")
}

func TestReadbackReferenceMainComparisonHasPositiveMatchedMixAndStrictNegative(t *testing.T) {
	corpus := Corpus{Label: "primary", Source: "/pinned", Commit: "0123456789012345678901234567890123456789", File: "main.go", Symbol: "main.go::Target", Query: "Target", ProposedSignature: "func Target() int"}
	ops, err := readingPlan(corpus)
	require.NoError(t, err)
	fixture := func(revision string, latency float64) Report {
		r := Report{SchemaVersion: reportVersion, GitSHA: revision, Complete: true, Profile: "agent", Seed: 1, Host: Host{Machine: "same", OS: "linux", Arch: "arm64", Kernel: "same", Profile: "reference", Validated: true}, Corpus: []Corpus{corpus}, Workload: Workload{Scenario: "read", Agents: 4, Profile: "agent", Seed: 1, DurationSec: 60, SessionModel: "per_agent", Operations: ops}, Phases: []Phase{{Name: "read", SessionModel: "per_agent"}}}
		r.RequestPlan, _ = hashJSON(r.Workload)
		for operation, op := range ops {
			for n := 0; n < 100; n++ {
				r.Phases[0].Attempts = append(r.Phases[0].Attempts, Attempt{Role: "workload", Tool: op.name(), Agent: n % 4, Step: operation*100 + n, Request: op.name(), Outcome: "success", LogicalMS: latency})
			}
		}
		return r
	}
	const mainPin = "1111111111111111111111111111111111111111"
	const candidatePin = "2222222222222222222222222222222222222222"
	main, candidate := fixture(mainPin, 10), fixture(candidatePin, 10.4)
	require.NoError(t, compareReadbackReferenceMain(main, candidate, mainPin, candidatePin))
	candidate = fixture(candidatePin, 10.6)
	require.ErrorContains(t, compareReadbackReferenceMain(main, candidate, mainPin, candidatePin), "5%")
	candidate = fixture(candidatePin, 10.4)
	candidate.Host.Machine = "foreign"
	require.Error(t, compareReadbackReferenceMain(main, candidate, mainPin, candidatePin))
}
