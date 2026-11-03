package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func readbackActualArrivalFixture(t *testing.T) (readbackReferenceSchedule, []readbackReferenceRecord, time.Time) {
	t.Helper()
	raw, err := os.ReadFile("testdata/readback-reference/observed-arrival-local-symbol.json")
	require.NoError(t, err)
	var fixture struct {
		Records []readbackReferenceRecord `json:"records"`
		Started time.Time                 `json:"started"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	schedule := readbackReferenceSchedule{Cell: readbackReferenceCell{Agents: 1, Kind: "modify_body", Tool: "edit.symbol", Offers: 100}, Arrival: []editProbeArrival{{Editor: 0, DueNS: 0}}}
	return schedule, fixture.Records, fixture.Started
}

func TestReadbackReferenceActualArrivalRequiresRecordedWritesAndFrozenIDs(t *testing.T) {
	schedule, rows, started := readbackActualArrivalFixture(t)
	hash, err := readbackReferenceActualArrivalSHA(schedule, rows, "under_edit", readbackReferenceCompatibilitySurface, started)
	require.NoError(t, err)
	require.Len(t, hash, 64)
	// A refusal is still an offered write. Correctness/quantile checks retain
	// it independently; the offer identity must not drop it from the stream.
	refused := rows[0].Probes["source"]
	refused.Correct, refused.Error = false, "explicit refusal"
	rows[0].Probes["source"] = refused
	actual, err := readbackReferenceActualArrivalSHA(schedule, rows, "under_edit", readbackReferenceCompatibilitySurface, started)
	require.NoError(t, err)
	require.Equal(t, hash, actual)
	for _, mutation := range []string{"missing_row", "due", "editor", "ordinal", "session", "surface", "censored", "missing_rpc", "missing_write", "early_write", "early_origin", "reversed_write", "duplicate_rpc", "missing_ack"} {
		t.Run(mutation, func(t *testing.T) {
			schedule, rows, started := readbackActualArrivalFixture(t)
			switch mutation {
			case "missing_row":
				rows = nil
			case "due":
				rows[0].DueNS++
			case "editor":
				rows[0].Editor++
			case "ordinal":
				rows[0].Ordinal++
			case "session":
				rows[0].Session = "foreign-editor"
			case "surface":
				rows[0].ToolSurface = readbackReferenceFacadeSurface
			case "censored":
				rows[0].ProbeOrigin = time.Time{}
			case "early_origin":
				rows[0].ProbeOrigin = started.Add(-time.Second)
			case "missing_ack":
				rows[0].ACK = readbackReferenceRPC{}
			default:
				rpc := rows[0].Probes["source"]
				switch mutation {
				case "missing_rpc":
					rpc.ID = 0
				case "missing_write":
					rpc.WriteStarted = time.Time{}
				case "early_write":
					rpc.WriteStarted = started.Add(-time.Second)
				case "reversed_write":
					rpc.WriteFinished = rpc.WriteStarted.Add(-time.Second)
				case "duplicate_rpc":
					rpc.ID = rows[0].Probes["symbol"].ID
				}
				rows[0].Probes["source"] = rpc
			}
			hash, err := readbackReferenceActualArrivalSHA(schedule, rows, "under_edit", readbackReferenceCompatibilitySurface, started)
			require.Error(t, err)
			require.Empty(t, hash, "unknown actual execution cannot borrow a seed/config hash")
		})
	}
}

func TestReadbackReferenceArrivalHashNormalizesOnlyEphemeralWireIDs(t *testing.T) {
	schedule, rows, started := readbackActualArrivalFixture(t)
	under, err := readbackReferenceActualArrivalSHA(schedule, rows, "under_edit", readbackReferenceCompatibilitySurface, started)
	require.NoError(t, err)
	// This is a conditional source-control fixture, not a claimed actual
	// no-edit run. The baseline needs its own real records before reuse.
	rows[0].ACK = readbackReferenceRPC{}
	for name, rpc := range rows[0].Probes {
		rpc.ID += 100
		rows[0].Probes[name] = rpc
	}
	baseline, err := readbackReferenceActualArrivalSHA(schedule, rows, "no_edit", readbackReferenceCompatibilitySurface, started)
	require.NoError(t, err)
	require.Equal(t, under, baseline)
	second := rows[0]
	second.Ordinal, second.DueNS = 1, 100
	rows = append(rows, second)
	schedule.Arrival = append(schedule.Arrival, editProbeArrival{Editor: 0, DueNS: 100})
	_, err = readbackReferenceActualArrivalSHA(schedule, rows, "no_edit", readbackReferenceCompatibilitySurface, started)
	require.Error(t, err, "another logical offer cannot replay prior per-connection RPC identities")
}
