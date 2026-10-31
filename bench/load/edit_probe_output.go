package main

import "fmt"

func editProbeOutputMode(c Corpus) (string, error) {
	switch c.EditOutput {
	case "", "json":
		return "json", nil
	case "toon":
		return "toon", nil
	default:
		return "", fmt.Errorf("unsupported edit output mode %q (want json or toon)", c.EditOutput)
	}
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
