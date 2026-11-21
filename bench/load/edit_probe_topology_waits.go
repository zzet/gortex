package main

import "slices"

type editProbePatchOwner struct {
	route, key    string
	ticket, retry uint64
}

type editProbePatchWaits struct {
	owner   editProbePatchOwner
	gate    Metric
	resolve Metric
}

type editProbeTopologyWaitSummary struct {
	PhysicalPatches    int
	UnboundEdits       int
	ConflictingPatches int
	Gate               editProbeQuantiles
	Resolve            editProbeQuantiles
}

func (p editProbePlan) sixRepoTopology() bool {
	return p.LinkedWorktrees == 0 && len(p.Routes) == 6 && p.Editors == 6
}

// Bind wait observations to the terminal receipt and its accepted patch. A
// coalesced receipt may point to the same serving patch as another receipt.
func readEditProbePatchWaits(status map[string]any, route, mutationID, receipt string) *editProbePatchWaits {
	if route == "" || receipt == "" || mutationID == "" || status["receipt"] != receipt || status["mutation_id"] != mutationID ||
		status["found"] != true || status["disk_status"] != "committed" ||
		status["graph_status_terminal"] != true || status["graph_status"] != "fresh" {
		return nil
	}
	requested, requestErr := strconvEditProbeUint(status["reindex_generation"])
	applied, appliedErr := strconvEditProbeUint(status["applied_generation"])
	if requestErr != nil || appliedErr != nil || requested == 0 || applied < requested {
		return nil
	}
	publication, _ := status["publication_phases"].(map[string]any)
	physical, _ := status["phase_timing"].(map[string]any)
	source := "mcp_edit"
	if applied > requested {
		winner, _ := status["coalesced_publication"].(map[string]any)
		winnerRequested, _ := strconvEditProbeUint(winner["requested_generation"])
		winnerApplied, _ := strconvEditProbeUint(winner["applied_generation"])
		originalTicket, _ := strconvEditProbeUint(publication["ticket"])
		serving, _ := winner["publication_phases"].(map[string]any)
		originalKey, _ := publication["key"].(string)
		servingKey, _ := serving["key"].(string)
		if winner["scope"] != "independent_physical_winner_serving_coalesced_receipt" ||
			winnerRequested != requested || winnerApplied != applied || originalTicket != requested ||
			publication["source"] != source || originalKey == "" || originalKey == servingKey ||
			physical["patch_accounting"] != nil || physical["patch_wall_ms"] != nil || physical["resolve_mutex_wait"] != nil {
			return nil
		}
		source = "canonical_coalesced_attempt"
		timings, _ := winner["timings"].(map[string]any)
		publication = serving
		physical, _ = timings["phase_timing"].(map[string]any)
	}
	accounting, ok := editProbePhysicalAccounting(physical, publication, applied)
	if !ok || publication["source"] != source {
		return nil
	}
	key, _ := publication["key"].(string)
	retry, _ := strconvEditProbeUint(accounting["attempt_retry"])
	out := &editProbePatchWaits{
		owner:   editProbePatchOwner{route: route, key: key, ticket: applied, retry: retry},
		gate:    unavailable("ms", "physical topology-gate wait unavailable"),
		resolve: unavailable("ms", "physical resolve-mutex wait unavailable"),
	}
	wall, _ := editProbeNS(physical["patch_wall_ms"])
	phases, _ := accounting["phases_ms"].(map[string]any)
	states, _ := accounting["phase_states"].(map[string]any)
	if gate, err := editProbeNS(phases["gate_wait"]); err == nil && gate <= wall && states["gate_wait"] == "measured" {
		out.gate = measured(float64(gate)/1e6, "ms")
	}
	wait, _ := physical["resolve_mutex_wait"].(map[string]any)
	acquisitions, countErr := strconvEditProbeUint(wait["acquisitions"])
	hold, holdErr := editProbeNS(wait["milliseconds"])
	resolve, resolveErr := editProbeNS(phases["resolve"])
	if wait["state"] == "measured" && wait["subset_of"] == "resolve" && countErr == nil && acquisitions > 0 &&
		holdErr == nil && resolveErr == nil && states["resolve"] == "measured" && hold <= resolve && resolve <= wall {
		out.resolve = measured(float64(hold)/1e6, "ms")
	}
	return out
}

func summarizeEditProbeTopologyWaits(records []editProbeRecord, seed int64) *editProbeTopologyWaitSummary {
	out := &editProbeTopologyWaitSummary{}
	owners := make(map[editProbePatchOwner]*editProbePatchWaits)
	for _, record := range records {
		wait := record.patchWaits
		if wait == nil || record.Outcome == "harness_evidence_incomplete" {
			out.UnboundEdits++
			continue
		}
		if prior, ok := owners[wait.owner]; !ok {
			owners[wait.owner] = wait
		} else if prior != nil && (!sameEditProbeWait(prior.gate, wait.gate) || !sameEditProbeWait(prior.resolve, wait.resolve)) {
			owners[wait.owner] = nil
			out.ConflictingPatches++
		}
	}
	var gate, resolve []float64
	for _, wait := range owners {
		if wait == nil {
			continue
		}
		if wait.gate.Value != nil {
			gate = append(gate, *wait.gate.Value)
		}
		if wait.resolve.Value != nil {
			resolve = append(resolve, *wait.resolve.Value)
		}
	}
	slices.Sort(gate)
	slices.Sort(resolve)
	out.PhysicalPatches = len(owners)
	out.Gate = editProbeQuantilesOf(gate, len(owners)-len(gate), 0, "ms", seed, "physical_topology_gate_wait_ms")
	out.Resolve = editProbeQuantilesOf(resolve, len(owners)-len(resolve), 0, "ms", seed, "physical_resolve_mutex_wait_ms")
	return out
}

func appendEditProbeTopologyWaits(phase *Phase, seed int64) {
	waits := summarizeEditProbeTopologyWaits(phase.EditProbe.Records, seed)
	phase.EditProbe.Summary.Measures["physical_topology_gate_wait_ms"] = waits.Gate
	phase.EditProbe.Summary.Measures["physical_resolve_mutex_wait_ms"] = waits.Resolve
	phase.Metrics["physical_topology_wait_patches"] = measured(float64(waits.PhysicalPatches), "patches")
	phase.Metrics["physical_topology_wait_unbound_edits"] = measured(float64(waits.UnboundEdits), "edits")
	phase.Metrics["physical_topology_wait_conflicting_patches"] = measured(float64(waits.ConflictingPatches), "patches")
}

func sameEditProbeWait(a, b Metric) bool {
	if a.Value == nil || b.Value == nil {
		return a.Value == nil && b.Value == nil
	}
	return *a.Value == *b.Value
}
