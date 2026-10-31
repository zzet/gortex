package main

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
)

// Every oracle reports into exactly one visibility group: the edited file's
// own graph, its call relations, or search.
var editProbeOracleGroups = map[string]string{
	"source": "file_graph", "summary": "file_graph", "editing_context": "file_graph", "old_source_absent": "file_graph",
	"callers": "relations", "call_chain": "relations",
	"search_visible": "search", "text": "search",
}

// A group is visible when its slowest member first answered correctly; a
// member that never did leaves the group unavailable rather than faster.
func editProbeGroupTime(firstCorrect map[string]Metric, group string) Metric {
	var latest *float64
	members := 0
	for _, name := range slices.Sorted(maps.Keys(firstCorrect)) {
		if editProbeOracleGroups[name] != group {
			continue
		}
		members++
		value := firstCorrect[name].Value
		if value == nil {
			return unavailable("ms", name+" never answered correctly")
		}
		if latest == nil || *value > *latest {
			latest = value
		}
	}
	if members == 0 {
		return unavailable("ms", "cell has no "+group+" oracle")
	}
	return measured(*latest, "ms")
}

// The derived step runs after every client-visible answer can already be
// correct, so no probe observes it. It is timed on the daemon's own clock,
// from the receipt origin to patch_derived_finished in this record's own
// publication timeline, and is not additive with client-side times. A
// coalesced receipt's derived step ran in the serving publication's timeline.
func editProbeDerived(status map[string]any) Metric {
	if _, coalesced := status["coalesced_publication"]; coalesced {
		return unavailable("ms", "coalesced receipt: derived ran in the serving publication's timeline")
	}
	timeline, ok := status["publication_phases"].(map[string]any)
	if !ok {
		return unavailable("ms", "status exposes no publication_phases")
	}
	if timeline["clock"] != "monotonic" || timeline["terminal"] != true {
		return unavailable("ms", "publication timeline is not terminal on a monotonic clock")
	}
	phases, _ := timeline["phases"].([]any)
	offsets := map[string]int64{}
	for _, entry := range phases {
		row, _ := entry.(map[string]any)
		name, _ := row["phase"].(string)
		if name != "received" && name != "patch_derived_finished" {
			continue
		}
		ns, err := strconv.ParseInt(fmt.Sprint(row["offset_ns"]), 10, 64)
		if _, repeated := offsets[name]; err != nil || repeated {
			return unavailable("ms", "invalid or repeated "+name+" offset")
		}
		offsets[name] = ns
	}
	origin, received := offsets["received"]
	finished, derived := offsets["patch_derived_finished"]
	switch {
	case !received || !derived:
		return unavailable("ms", "publication timeline has no received/derived pair")
	case finished < origin:
		return unavailable("ms", "derived step precedes receipt origin")
	}
	return measured(float64(finished-origin)/1e6, "ms")
}
