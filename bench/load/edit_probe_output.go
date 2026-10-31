package main

import "fmt"

func editProbeOutputMode(c Corpus) (string, error) {
	switch c.EditOutput {
	case "", "json":
		return "json", nil
	case "toon":
		return "toon", nil
	case "gcx":
		return "", fmt.Errorf("edit output mode gcx: %w", errEditOutputGCXUnavailable)
	default:
		return "", fmt.Errorf("unsupported edit output mode %q (want json or toon)", c.EditOutput)
	}
}

// Every output mode is accounted in each report, including the ones this
// harness cannot select, so absent coverage is explicit rather than silent.
var editProbeOutputModes = []string{"json", "toon", "compact", "cli", "gcx"}

type editProbeModes map[string]editProbeModeCoverage

type editProbeModeCoverage struct {
	State                 string `json:"state"`
	Reason                string `json:"reason,omitempty"`
	ProbeAnswers          int    `json:"probe_answers"`
	DecodeFailures        int    `json:"decode_failures"`
	ClaimsFreshObservable bool   `json:"claims_fresh_observable"`
	ClaimsFresh           int    `json:"claims_fresh"`
	StaleClaimedFresh     Metric `json:"stale_claimed_fresh"`
	PrecommitMismatches   int    `json:"precommit_fresh_mismatches_unassessed"`
}

type editProbeModeCounts struct {
	answers, decodeFailures, claimsFresh, stale, precommit int
}

// Answers count by each oracle request's actual output mode. A mode without
// answers, or whose payload cannot carry a freshness claim, reports its stale
// claimed-fresh count unavailable rather than zero.
func editProbeOutputCoverage(selected string, counts map[string]editProbeModeCounts) editProbeModes {
	out := make(editProbeModes, len(editProbeOutputModes))
	for _, mode := range editProbeOutputModes {
		count := counts[mode]
		coverage := editProbeModeCoverage{State: "not_selected", ProbeAnswers: count.answers, DecodeFailures: count.decodeFailures, ClaimsFreshObservable: true, ClaimsFresh: count.claimsFresh, PrecommitMismatches: count.precommit}
		switch {
		case mode == "gcx":
			coverage.State, coverage.Reason, coverage.ClaimsFreshObservable = "unavailable", errEditOutputGCXUnavailable.Error(), false
		case mode == "compact" || mode == "cli":
			coverage.State, coverage.Reason, coverage.ClaimsFreshObservable = "unavailable", "not implemented by this harness", false
		case mode == selected:
			coverage.State = "selected"
		}
		switch {
		case !coverage.ClaimsFreshObservable:
			coverage.StaleClaimedFresh = unavailable("answers", "mode cannot carry a freshness claim here")
		case count.answers == 0:
			coverage.StaleClaimedFresh = unavailable("answers", "no probe answers in this mode")
		default:
			coverage.StaleClaimedFresh = measured(float64(count.stale), "answers")
		}
		out[mode] = coverage
	}
	return out
}

func (r *editProbeRuntime) countMode(mode string, answered, decoded, fresh, stale, precommit bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.modeCounts == nil {
		r.modeCounts = map[string]editProbeModeCounts{}
	}
	count := r.modeCounts[mode]
	if answered {
		count.answers++
		if !decoded {
			count.decodeFailures++
		}
	}
	if fresh {
		count.claimsFresh++
	}
	if stale {
		count.stale++
	}
	if precommit {
		count.precommit++
	}
	r.modeCounts[mode] = count
}

func editProbeRequestOutput(req request) string {
	output, exists := req.Arguments["output"]
	if !exists {
		return "json"
	}
	fields, ok := output.(map[string]any)
	if !ok {
		return "invalid"
	}
	format, exists := fields["format"]
	if !exists {
		return "json"
	}
	value, ok := format.(string)
	if !ok {
		return "invalid"
	}
	return value
}
func editProbeWithOutput(req request, format string) request {
	if format == "toon" {
		req.Arguments = copyJSON(req.Arguments)
		if req.Arguments == nil {
			req.Arguments = make(map[string]any)
		}
		req.Arguments["output"] = map[string]any{"format": "toon", "max_bytes": 0}
	}
	return req
}
func editProbeOutputOracles(c Corpus, oracles []editProbeOracle) []editProbeOracle {
	if c.EditOutput != "toon" {
		return oracles
	}
	for i := range oracles {
		oracles[i].call = editProbeWithOutput(oracles[i].call, "toon")
	}
	return oracles
}

// This certifies only the actual format's complete emitted row census. It
// never inserts a JSON total_nodes/total_edges value that TOON did not serve.
// The known compact traversal producer emits total+truncated, retains all edge
// rows, and omits total_edges precisely when no edge rows were clipped.
func editProbeTOONCompleteRows(body map[string]any, key, total string) bool {
	proof, ok := body[editProbeTOONWireKey].(editProbeTOONWire)
	if !ok || body["truncated"] != false {
		return false
	}
	switch proof.operation {
	case "relations.callers", "trace.call_chain":
	default:
		return false
	}
	if truncated, exists := body["_truncated_by_budget"]; exists && truncated != false {
		return false
	}
	nodes, exists := body["nodes"]
	rows, array := nodes.([]any)
	if exists && nodes == nil {
		rows, array = []any{}, true
	}
	declared, err := strconvEditProbeUint(body["total"])
	if !exists || !array || err != nil || declared != uint64(len(rows)) {
		return false
	}
	return key == "nodes" && total == "total_nodes" || key == "edges" && total == "total_edges"
}
