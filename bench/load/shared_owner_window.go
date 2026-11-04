package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
)

var editProbeSharedPhaseNames = [...]string{
	"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "patch_analysis",
	"attempt_setup", "patch_inspection", "executor_control", "attempt_finalize", "cohort_admission", "batch_admission", "lane_wait",
}

func editProbeExactKeys(object map[string]any, names ...string) bool {
	if len(object) != len(names) {
		return false
	}
	for _, name := range names {
		if _, present := object[name]; !present {
			return false
		}
	}
	return true
}

func editProbeSharedInteger(value any) (int64, error) {
	n, err := strconvEditProbeUint(value)
	if err != nil || n > math.MaxInt64 {
		return 0, errors.New("missing/noncanonical nonnegative int64")
	}
	return int64(n), nil
}

// This validates one already selected owner's E partition. It grants no owner,
// membership or daemon/arm authority; the enclosing proof must validate those
// separately before a report can credit this window. In particular, the old
// point branch never calls this direct-set applicability rule.
func checkEditProbeSharedExecution(execution map[string]any, members int) (editProbeWindow, error) {
	invalid := func(reason string) (editProbeWindow, error) {
		return editProbeWindow{}, errors.New(reason)
	}
	if !editProbeExactKeys(execution, "clock", "origin", "entry_disposition", "start_ns", "end_ns", "observed_partition_complete", "phases_ms", "phase_states", "phase_dispositions", "intervals") ||
		execution["clock"] != "monotonic" || execution["origin"] != "actual_callback_entry" || execution["entry_disposition"] != "direct_shared_set" || execution["observed_partition_complete"] != true {
		return invalid("shared execution scope/disposition/completeness unavailable")
	}
	start, err := editProbeSharedInteger(execution["start_ns"])
	if err != nil || start != 0 {
		return invalid("shared execution origin is not actual callback zero")
	}
	wall, err := editProbeSharedInteger(execution["end_ns"])
	if err != nil {
		return invalid("shared execution end unavailable")
	}
	phases, _ := execution["phases_ms"].(map[string]any)
	states, _ := execution["phase_states"].(map[string]any)
	if !editProbeExactKeys(phases, editProbeSharedPhaseNames[:]...) || !editProbeExactKeys(states, editProbeSharedPhaseNames[:]...) {
		return invalid("shared execution phase set differs from the declared partition")
	}
	dispositions, ok := execution["phase_dispositions"].(map[string]any)
	if !ok || dispositions == nil {
		return invalid("shared execution actual optional branch dispositions unavailable")
	}
	optionalReasons := map[string]string{"patch_analysis": "resolution_catchup_not_entered", "batch_admission": "no_batch_gate_or_hook"}
	for name, reason := range dispositions {
		expected, optional := optionalReasons[name]
		if !optional || reason != expected || states[name] != "not_applicable" || phases[name] != nil {
			return invalid("shared execution has false optional branch disposition: " + name)
		}
	}
	intervals, ok := execution["intervals"].([]any)
	if !ok || members <= 0 || members > math.MaxInt/(len(editProbeSharedPhaseNames)*8) || len(intervals) > members*len(editProbeSharedPhaseNames)*8 {
		return invalid("shared execution intervals absent or beyond the complete frontier bound")
	}
	sums := make(map[string]int64, len(phases))
	counts := make(map[string]int, len(phases))
	var previousEnd int64
	for _, value := range intervals {
		interval, _ := value.(map[string]any)
		if !editProbeExactKeys(interval, "phase", "start_ns", "end_ns") {
			return invalid("shared execution interval shape unavailable")
		}
		name, _ := interval["phase"].(string)
		if _, present := phases[name]; !present || states[name] != "measured" {
			return invalid("shared execution interval has unknown or inapplicable phase")
		}
		begin, beginErr := editProbeSharedInteger(interval["start_ns"])
		end, endErr := editProbeSharedInteger(interval["end_ns"])
		if beginErr != nil || endErr != nil || begin < previousEnd || end < begin || end > wall || sums[name] > math.MaxInt64-(end-begin) {
			return invalid("shared execution intervals overlap or escape their owner window")
		}
		sums[name] += end - begin
		counts[name]++
		previousEnd = end
	}
	for _, name := range editProbeSharedPhaseNames {
		// These two operations are absent only on the producer-proven direct
		// entry. Even measured zero would falsely borrow a point-path event.
		directAbsent := name == "cohort_admission" || name == "patch_inspection"
		reason, optional := optionalReasons[name]
		if directAbsent || optional && states[name] == "not_applicable" {
			if states[name] != "not_applicable" || phases[name] != nil || counts[name] != 0 || optional && dispositions[name] != reason {
				return invalid("shared execution has contradictory applicability: " + name)
			}
			continue
		}
		_, numeric := phases[name].(json.Number)
		ns, err := editProbeNS(phases[name])
		if !numeric || err != nil || states[name] != "measured" || counts[name] == 0 || sums[name] != ns {
			return invalid("shared execution lacks exact measured interval sum: " + name)
		}
	}
	window, err := checkEditProbeWindowNS(wall, phases)
	if err != nil {
		return window, fmt.Errorf("shared execution E: %w", err)
	}
	return window, nil
}

// B has one reference to E, not another copy of E's additive phase values.
// Every local interval below is an explicit producer observation. The final
// interval must be the observed execution-return to drain-return control span;
// this validator never invents it by subtracting E from a callback envelope.
func checkEditProbeSharedBackground(background, owner map[string]any, execution editProbeWindow) (editProbeWindow, error) {
	invalid := func(reason string) (editProbeWindow, error) {
		return editProbeWindow{}, errors.New(reason)
	}
	if !editProbeExactKeys(background, "clock", "origin", "start_ns", "end_ns", "end_boundary", "excluded_work", "observed_partition_complete", "local_intervals", "execution_ref", "post_execution_control") ||
		background["clock"] != "monotonic" || background["origin"] != "actual_original_set_scheduler_entry" || background["end_boundary"] != "drain_return" || background["observed_partition_complete"] != true {
		return invalid("shared background scope/boundary/completeness unavailable")
	}
	excluded, ok := background["excluded_work"].([]any)
	if !ok || len(excluded) != 2 || excluded[0] != "owner_proof_projection" || excluded[1] != "outer_timer_work_done" {
		return invalid("shared background excludes different work")
	}
	start, err := editProbeSharedInteger(background["start_ns"])
	if err != nil || start != 0 {
		return invalid("shared background origin is not original scheduler zero")
	}
	wall, err := editProbeSharedInteger(background["end_ns"])
	if err != nil {
		return invalid("shared background end unavailable")
	}
	rows, ok := background["local_intervals"].([]any)
	if !ok || len(rows) != 3 {
		return invalid("shared background local partition unavailable")
	}
	var end int64
	for i, name := range []string{"scheduling_control", "timer_arm", "quiet_wait"} {
		row, _ := rows[i].(map[string]any)
		if !editProbeExactKeys(row, "phase", "start_ns", "end_ns") || row["phase"] != name {
			return invalid("shared background contains unknown/double-counted local work")
		}
		begin, beginErr := editProbeSharedInteger(row["start_ns"])
		next, nextErr := editProbeSharedInteger(row["end_ns"])
		if beginErr != nil || nextErr != nil || begin != end || next < begin || next > wall {
			return invalid("shared background local intervals overlap or leave gaps")
		}
		end = next
	}
	ref, _ := background["execution_ref"].(map[string]any)
	if len(owner) == 0 || !editProbeExactKeys(ref, "owner", "window", "start_ns", "end_ns") || ref["window"] != "execution" || !reflect.DeepEqual(ref["owner"], owner) {
		return invalid("shared background refers to a different/missing execution owner")
	}
	begin, beginErr := editProbeSharedInteger(ref["start_ns"])
	next, nextErr := editProbeSharedInteger(ref["end_ns"])
	if beginErr != nil || nextErr != nil || begin != end || next < begin || next > wall || next-begin != execution.WallNS || !execution.WithinFivePercent {
		return invalid("shared background execution reference differs from actual E")
	}
	post, _ := background["post_execution_control"].(map[string]any)
	if !editProbeExactKeys(post, "start_ns", "end_ns") {
		return invalid("shared background actual post-execution control unavailable")
	}
	postStart, startErr := editProbeSharedInteger(post["start_ns"])
	postEnd, endErr := editProbeSharedInteger(post["end_ns"])
	if startErr != nil || endErr != nil || postStart != next || postEnd < postStart || postEnd != wall {
		return invalid("shared background post-execution control does not end at C1")
	}
	return editProbeWindow{WallNS: wall, SumNS: wall, WithinFivePercent: true}, nil
}
