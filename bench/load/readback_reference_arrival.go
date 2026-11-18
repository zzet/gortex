package main

import (
	"errors"
	"fmt"
	"time"
)

// Hash the observed logical offer IDs only after matching their due/editor
// stream to the frozen plan and proving real writes on the assigned connection.
// Ephemeral RPC IDs and wall-clock lateness stay in the records, not the hash:
// a no-edit baseline has no ACK RPC and independently delivered probes.
func readbackReferenceActualArrivalSHA(schedule readbackReferenceSchedule, rows []readbackReferenceRecord, role, surface string, started time.Time) (string, error) {
	if started.IsZero() || len(schedule.Arrival) == 0 || len(rows) != len(schedule.Arrival) || role != "under_edit" && role != "no_edit" {
		return "", errors.New("offered stream unavailable: missing actual rows or role")
	}
	if _, err := readbackReferenceSurface(surface); err != nil {
		return "", err
	}
	type offer struct {
		Ordinal int   `json:"ordinal"`
		Editor  int   `json:"editor"`
		DueNS   int64 `json:"due_ns"`
	}
	actual := make([]offer, len(rows))
	seenWire := map[string]bool{}
	for i, row := range rows {
		expected := schedule.Arrival[i]
		if row.Ordinal != i || row.Editor != expected.Editor || row.DueNS != expected.DueNS || row.DueNS < 0 || row.Editor < 0 || row.Editor >= schedule.Cell.Agents || row.ProbeOrigin.IsZero() || row.ProbeOrigin.Before(started.Add(time.Duration(row.DueNS))) || row.StartLatenessNS < 0 || row.Session != fmt.Sprintf("fixed-editor-%d", row.Editor) || row.ToolSurface != surface {
			return "", fmt.Errorf("offered stream unavailable: row%d missing or contradicts frozen offer identity", i)
		}
		written := func(rpc readbackReferenceRPC) bool {
			key := fmt.Sprintf("%d/%d", row.Editor, rpc.ID)
			if rpc.ID <= 0 || len(rpc.Request) == 0 || rpc.WriteStarted.IsZero() || rpc.WriteFinished.IsZero() || rpc.WriteFinished.Before(rpc.WriteStarted) || rpc.WriteStarted.Before(started.Add(time.Duration(row.DueNS))) || seenWire[key] {
				return false
			}
			seenWire[key] = true
			return true
		}
		if role == "under_edit" && !written(row.ACK) {
			return "", fmt.Errorf("offered stream unavailable: row%d ACK write identity missing or reused", i)
		}
		if role == "no_edit" && (row.ACK.ID != 0 || !row.ACK.WriteStarted.IsZero() || len(row.ACK.Request) != 0) {
			return "", fmt.Errorf("offered stream unavailable: row%d baseline contains a mutation write", i)
		}
		endpoints := []string{"source", "summary", "editing_context", "file"}
		if surface == readbackReferenceCompatibilitySurface {
			endpoints = append(endpoints, "symbol")
		}
		for _, name := range endpoints {
			rpc, present := row.Probes[name]
			if !present || !written(rpc) {
				return "", fmt.Errorf("offered stream unavailable: row%d %s write identity missing or reused", i, name)
			}
		}
		actual[i] = offer{Ordinal: row.Ordinal, Editor: row.Editor, DueNS: row.DueNS}
	}
	return hashJSON(actual)
}
