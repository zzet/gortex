package main

import (
	"cmp"
	"fmt"
	"math"
	"strconv"
)

// The daemon's ACK laps followed by its patch laps, as mutation_status names
// them in phases_ms.
var editProbePhaseNames = []string{
	"dispatcher_queue", "middleware", "parse", "disk_write", "publish", "reindex_wait", "edit_control", "caller_result",
	"debounce", "cohort_admission", "batch_admission", "lane_wait", "gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived",
}

// Every expected lap is reported for every record. A status without phases_ms
// (a build that predates it, or no status at all) leaves each lap unavailable
// with that reason, and a producer null keeps the producer's own phase state as
// the reason. Missing laps are never reported as a measured zero.
func editProbePhases(status map[string]any, missing string) map[string]Metric {
	phases, present := status["phases_ms"].(map[string]any)
	states, _ := status["phase_states"].(map[string]any)
	out := make(map[string]Metric, len(editProbePhaseNames))
	for _, name := range editProbePhaseNames {
		value, listed := phases[name]
		state, _ := states[name].(string)
		switch {
		case !present:
			out[name] = unavailable("ms", missing)
		case !listed:
			out[name] = unavailable("ms", "status phases_ms omits "+name)
		case value == nil:
			out[name] = unavailable("ms", "producer phase state "+cmp.Or(state, "unreported"))
		default:
			ms, err := strconv.ParseFloat(fmt.Sprint(value), 64)
			if err != nil || ms < 0 || math.IsInf(ms, 0) || math.IsNaN(ms) {
				out[name] = unavailable("ms", "invalid phases_ms value")
			} else {
				out[name] = measured(ms, "ms")
			}
		}
	}
	return out
}
