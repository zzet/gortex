package main

import (
	"encoding/json"
	"testing"
)

func topologyWaitStatus() map[string]any {
	return map[string]any{
		"found": true, "receipt": "receipt-a", "mutation_id": "edit-a",
		"disk_status": "committed", "graph_status": "fresh", "graph_status_terminal": true,
		"reindex_generation": json.Number("7"), "applied_generation": json.Number("7"),
		"publication_phases": map[string]any{
			"clock": "monotonic", "terminal": true, "key": "publication-a",
			"source": "mcp_edit", "ticket": json.Number("7"),
		},
		"phase_timing": map[string]any{
			"clock": "monotonic", "patch_wall_ms": json.Number("20"),
			"patch_accounting": map[string]any{
				"scope": "accepted_winning_physical_attempt", "attempt_ticket": json.Number("7"), "attempt_retry": json.Number("0"),
				"phases_ms":    map[string]any{"gate_wait": json.Number("0"), "resolve": json.Number("10")},
				"phase_states": map[string]any{"gate_wait": "measured", "resolve": "measured"},
			},
			"resolve_mutex_wait": map[string]any{
				"state": "measured", "subset_of": "resolve", "acquisitions": json.Number("2"), "milliseconds": json.Number("3"),
			},
		},
	}
}

func TestEditProbeTopologyWaitsBindToPhysicalPatch(t *testing.T) {
	status := topologyWaitStatus()
	read := func() *editProbePatchWaits {
		return readEditProbePatchWaits(status, "repo-a", "edit-a", "receipt-a")
	}
	got := read()
	if got == nil || got.owner.route != "repo-a" || got.owner.ticket != 7 ||
		got.gate.Value == nil || *got.gate.Value != 0 || got.resolve.Value == nil || *got.resolve.Value != 3 {
		t.Fatalf("direct physical waits = %+v", got)
	}
	physical := status["phase_timing"].(map[string]any)
	physical["resolve_mutex_wait"].(map[string]any)["milliseconds"] = json.Number("11")
	if got := read(); got == nil || got.gate.Value == nil || got.resolve.Value != nil {
		t.Fatalf("mutex exceeds its resolve lap: %+v", got)
	}
	delete(physical, "resolve_mutex_wait")
	if got := read(); got == nil || got.gate.Value == nil || got.resolve.Value != nil {
		t.Fatalf("absent mutex waits: %+v", got)
	}
	status["mutation_id"] = "another edit"
	if got := read(); got != nil {
		t.Fatalf("foreign receipt supplied a patch: %+v", got)
	}
}

func TestEditProbeTopologyWaitsUseAndDeduplicateIndependentWinner(t *testing.T) {
	status := topologyWaitStatus()
	winner := status["publication_phases"].(map[string]any)
	winner["source"], winner["key"] = "canonical_coalesced_attempt", "winning-physical"
	physical := status["phase_timing"].(map[string]any)
	status["reindex_generation"] = json.Number("6")
	status["publication_phases"] = map[string]any{"key": "original", "ticket": json.Number("6"), "source": "mcp_edit"}
	status["phase_timing"] = map[string]any{}
	status["coalesced_publication"] = map[string]any{
		"requested_generation": json.Number("6"), "applied_generation": json.Number("7"),
		"scope": "independent_physical_winner_serving_coalesced_receipt", "publication_phases": winner,
		"timings": map[string]any{"phase_timing": physical},
	}
	a := readEditProbePatchWaits(status, "repo-a", "edit-a", "receipt-a")
	if a == nil || a.owner.key != "winning-physical" || a.resolve.Value == nil || *a.resolve.Value != 3 {
		t.Fatalf("independent serving patch = %+v", a)
	}
	b := readEditProbePatchWaits(status, "repo-b", "edit-a", "receipt-a")
	b.resolve = unavailable("ms", "resolve mutex wait unavailable")
	records := []editProbeRecord{{patchWaits: a}, {patchWaits: a}, {patchWaits: b}, {}}
	got := summarizeEditProbeTopologyWaits(records, 42)
	if got.PhysicalPatches != 2 || got.UnboundEdits != 1 || got.Gate.Count != 2 ||
		got.Resolve.Count != 1 || got.Resolve.Censored != 1 || got.Resolve.P95.Value != nil {
		t.Fatalf("full deduplicated wait population = %+v", got)
	}
	conflict := *a
	conflict.gate = measured(1, "ms")
	got = summarizeEditProbeTopologyWaits(append(records, editProbeRecord{patchWaits: &conflict}), 42)
	if got.ConflictingPatches != 1 || got.Gate.Count != 1 || got.Resolve.Count != 0 {
		t.Fatalf("conflicting physical evidence qualified: %+v", got)
	}
	status["phase_timing"] = physical
	if got := readEditProbePatchWaits(status, "repo-a", "edit-a", "receipt-a"); got != nil {
		t.Fatalf("coalesced receipt borrowed direct physical waits: %+v", got)
	}
}

func TestEditProbeTopologyWaitsFinalizeOnlySixRepositorySixEditorCell(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		routes, editors, linked int
		want                    bool
	}{
		{name: "six repositories and six editors", routes: 6, editors: 6, want: true},
		{name: "six linked views", routes: 6, editors: 6, linked: 5},
		{name: "extra editors", routes: 6, editors: 7},
		{name: "single repository", editors: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase := &Phase{Metrics: map[string]Metric{}, EditProbe: &editProbeReport{}}
			run := &editProbeRuntime{
				p: &privateRun{Root: t.TempDir()}, phase: phase,
				plan: editProbePlan{Routes: make([]editProbeRoute, tc.routes), Editors: tc.editors, LinkedWorktrees: tc.linked},
			}
			if err := run.finish(); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"physical_topology_gate_wait_ms", "physical_resolve_mutex_wait_ms"} {
				got, present := phase.EditProbe.Summary.Measures[name]
				if present != tc.want || got.P95.Value != nil {
					t.Fatalf("%s has wrong scope or fabricated empty wait: %+v", name, got)
				}
			}
		})
	}
}
