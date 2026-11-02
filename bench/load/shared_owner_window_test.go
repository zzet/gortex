package main

import (
	"encoding/json"
	"strconv"
	"testing"
)

func sharedExecutionFixture() map[string]any {
	phases, states := map[string]any{}, map[string]any{}
	var intervals []any
	var end int64
	for _, name := range editProbeSharedPhaseNames {
		if name == "cohort_admission" || name == "patch_inspection" || name == "patch_analysis" || name == "batch_admission" {
			phases[name], states[name] = nil, "not_applicable"
			continue
		}
		phases[name], states[name] = json.Number("1"), "measured"
		intervals = append(intervals, map[string]any{"phase": name, "start_ns": json.Number(strconv.FormatInt(end, 10)), "end_ns": json.Number(strconv.FormatInt(end+1_000_000, 10))})
		end += 1_000_000
	}
	return map[string]any{"clock": "monotonic", "origin": "actual_callback_entry", "entry_disposition": "direct_shared_set", "start_ns": json.Number("0"), "end_ns": json.Number(strconv.FormatInt(end, 10)), "observed_partition_complete": true, "phases_ms": phases, "phase_states": states, "intervals": intervals,
		"phase_dispositions": map[string]any{"patch_analysis": "resolution_catchup_not_entered", "batch_admission": "no_batch_gate_or_hook"}}
}

// These are numerical wire controls only. Actual producer provenance, full
// membership and owned-process binding are independently required for credit.
func TestSharedExecutionChecksIntervalsAndDirectOnlyApplicability(t *testing.T) {
	if got, err := checkEditProbeSharedExecution(sharedExecutionFixture(), 2); err != nil || got.WallNS != 11_000_000 || got.SumNS != got.WallNS {
		t.Fatalf("complete direct-set numerical control rejected: %+v %v", got, err)
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing disposition", func(v map[string]any) { delete(v, "entry_disposition") }},
		{"unknown disposition", func(v map[string]any) { v["entry_disposition"] = "unknown" }},
		{"point cannot borrow direct exemption", func(v map[string]any) { v["entry_disposition"] = "point" }},
		{"missing entry cannot be inferred from scope", func(v map[string]any) { delete(v, "entry_disposition"); v["scope"] = "shared_mutation_set_executor" }},
		{"unobserved is not inapplicable", func(v map[string]any) { v["phase_states"].(map[string]any)["cohort_admission"] = "not_observed" }},
		{"numeric point zero is not direct applicability", func(v map[string]any) { v["phases_ms"].(map[string]any)["cohort_admission"] = json.Number("0") }},
		{"measured point inspection is not direct applicability", func(v map[string]any) {
			v["phase_states"].(map[string]any)["patch_inspection"] = "measured"
			v["phases_ms"].(map[string]any)["patch_inspection"] = json.Number("0")
		}},
		{"another required phase cannot be waived", func(v map[string]any) {
			v["phase_states"].(map[string]any)["resolve"] = "not_applicable"
			v["phases_ms"].(map[string]any)["resolve"] = nil
		}},
		{"missing required phase", func(v map[string]any) { delete(v["phases_ms"].(map[string]any), "resolve") }},
		{"unknown additive phase", func(v map[string]any) { v["phases_ms"].(map[string]any)["resolve_mutex_wait"] = json.Number("1") }},
		{"extra phase state", func(v map[string]any) { v["phase_states"].(map[string]any)["worker_parse"] = "measured" }},
		{"missing interval cannot become measured zero", func(v map[string]any) {
			v["intervals"] = v["intervals"].([]any)[1:]
			v["phases_ms"].(map[string]any)["gate_wait"] = json.Number("0")
		}},
		{"overlapping actual intervals", func(v map[string]any) { v["intervals"].([]any)[1].(map[string]any)["start_ns"] = json.Number("0") }},
		{"scalar disagrees with actual chunks", func(v map[string]any) { v["phases_ms"].(map[string]any)["resolve"] = json.Number("0.999999") }},
		{"interval escapes E", func(v map[string]any) {
			v["intervals"].([]any)[10].(map[string]any)["end_ns"] = json.Number("11000001")
		}},
		{"unknown interval phase", func(v map[string]any) { v["intervals"].([]any)[0].(map[string]any)["phase"] = "unclassified_work" }},
		{"point interval cannot be relabeled absent", func(v map[string]any) {
			v["intervals"] = append(v["intervals"].([]any), map[string]any{"phase": "patch_inspection", "start_ns": json.Number("11000000"), "end_ns": json.Number("11000000")})
		}},
		{"residual exceeds own five percent", func(v map[string]any) { v["end_ns"] = json.Number("12000000") }},
		{"producer incomplete", func(v map[string]any) { v["observed_partition_complete"] = false }},
		{"negative endpoint", func(v map[string]any) { v["end_ns"] = json.Number("-1") }},
		{"fractional endpoint", func(v map[string]any) { v["end_ns"] = json.Number("11000000.5") }},
		{"boolean endpoint", func(v map[string]any) { v["end_ns"] = true }},
		{"overflow endpoint", func(v map[string]any) { v["end_ns"] = json.Number("9223372036854775808") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := sharedExecutionFixture()
			tc.change(v)
			if got, err := checkEditProbeSharedExecution(v, 2); err == nil {
				t.Fatalf("invalid shared partition received numeric credit: %+v", got)
			}
		})
	}
}

func TestSharedExecutionRetainsRepeatedChunksAndObservedZero(t *testing.T) {
	v := sharedExecutionFixture()
	rows := v["intervals"].([]any)
	split := []any{
		map[string]any{"phase": "gate_wait", "start_ns": json.Number("0"), "end_ns": json.Number("400000")},
		map[string]any{"phase": "gate_wait", "start_ns": json.Number("400000"), "end_ns": json.Number("1000000")},
	}
	v["intervals"] = append(split, rows[1:]...)
	if _, err := checkEditProbeSharedExecution(v, 2); err != nil {
		t.Fatalf("actual repeated completed chunks rejected: %v", err)
	}
	for _, value := range v["intervals"].([]any) {
		row := value.(map[string]any)
		row["start_ns"], row["end_ns"] = json.Number("0"), json.Number("0")
		v["phases_ms"].(map[string]any)[row["phase"].(string)] = json.Number("0")
	}
	v["end_ns"] = json.Number("0")
	if got, err := checkEditProbeSharedExecution(v, 2); err != nil || got.WallNS != 0 || got.SumNS != 0 {
		t.Fatalf("observed zero was confused with absent evidence: %+v %v", got, err)
	}
	if _, err := checkEditProbeSharedExecution(v, 0); err == nil {
		t.Fatal("missing frontier must not credit even a zero partition")
	}
}

func TestSharedExecutionRequiresActualOptionalDisposition(t *testing.T) {
	v := sharedExecutionFixture()
	delete(v, "phase_dispositions")
	if got, err := checkEditProbeSharedExecution(v, 2); err == nil {
		t.Fatalf("optional null phases received credit without actual branch disposition: %+v", got)
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing actual branch reason", func(v map[string]any) { delete(v["phase_dispositions"].(map[string]any), "batch_admission") }},
		{"unknown reason", func(v map[string]any) {
			v["phase_dispositions"].(map[string]any)["batch_admission"] = "missing_measurement"
		}},
		{"mandatory phase cannot borrow optional reason", func(v map[string]any) {
			v["phase_dispositions"].(map[string]any)["resolve"] = "resolution_catchup_not_entered"
		}},
		{"direct entry exemption has its own proof", func(v map[string]any) {
			v["phase_dispositions"].(map[string]any)["cohort_admission"] = "no_batch_gate_or_hook"
		}},
		{"measured work cannot also be absent", func(v map[string]any) {
			v["phase_states"].(map[string]any)["patch_analysis"] = "measured"
			v["phases_ms"].(map[string]any)["patch_analysis"] = json.Number("0")
		}},
		{"unknown is not proven absent", func(v map[string]any) { v["phase_states"].(map[string]any)["batch_admission"] = "not_observed" }},
		{"null disposition object", func(v map[string]any) { v["phase_dispositions"] = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := sharedExecutionFixture()
			tc.change(v)
			if got, err := checkEditProbeSharedExecution(v, 2); err == nil {
				t.Fatalf("unsupported branch disposition received numeric credit: %+v", got)
			}
		})
	}
	// When both optional operations actually execute, an empty disposition
	// object and actual measured zero intervals are distinct from absence.
	v = sharedExecutionFixture()
	v["phase_dispositions"] = map[string]any{}
	for _, name := range []string{"patch_analysis", "batch_admission"} {
		v["phase_states"].(map[string]any)[name] = "measured"
		v["phases_ms"].(map[string]any)[name] = json.Number("0")
		v["intervals"] = append(v["intervals"].([]any), map[string]any{"phase": name, "start_ns": v["end_ns"], "end_ns": v["end_ns"]})
	}
	if _, err := checkEditProbeSharedExecution(v, 2); err != nil {
		t.Fatalf("actual optional zero measurements were treated as missing: %v", err)
	}
}

func TestSharedOwnerPathKeepsIndependentAcknowledgementFailures(t *testing.T) {
	status := editProbeAccountingFixture()
	point := checkEditProbeAccounting(status, "receipt-a")
	only := checkEditProbeAcknowledgement(status, "receipt-a", "reindex_generation", nil, false)
	if point.A == nil || only.A == nil || *point.A != *only.A || len(only.Reasons) != 0 || only.P != nil || only.B != nil {
		t.Fatalf("extracting A changed its measurements or borrowed P/B: point=%+v A=%+v", point, only)
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing bound A", func(v map[string]any) { delete(v, "acknowledgement") }},
		{"wrong receipt", func(v map[string]any) { v["receipt"] = "other" }},
		{"missing requested generation", func(v map[string]any) { delete(v, "reindex_generation") }},
		{"missing RPC owner", func(v map[string]any) { delete(v["acknowledgement"].(map[string]any), "rpc_id") }},
		{"unmeasured applicable parse", func(v map[string]any) {
			v["acknowledgement"].(map[string]any)["phase_states"].(map[string]any)["parse"] = "not_observed"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := editProbeAccountingFixture()
			tc.change(status)
			got := checkEditProbeAcknowledgement(status, "receipt-a", "reindex_generation", nil, false)
			if len(got.Reasons) == 0 || got.State != "unavailable" || got.P != nil || got.B != nil {
				t.Fatalf("a distinct E/B owner must never erase an A/identity failure: %+v", got)
			}
		})
	}
}

func sharedBackgroundFixture() (map[string]any, map[string]any, editProbeWindow) {
	owner := map[string]any{"test_owner": "same immutable owner"}
	row := func(name, begin, end string) any {
		return map[string]any{"phase": name, "start_ns": json.Number(begin), "end_ns": json.Number(end)}
	}
	background := map[string]any{
		"clock": "monotonic", "origin": "actual_original_set_scheduler_entry", "start_ns": json.Number("0"), "end_ns": json.Number("15000000"),
		"end_boundary": "drain_return", "excluded_work": []any{"owner_proof_projection", "outer_timer_work_done"}, "observed_partition_complete": true,
		"local_intervals":        []any{row("scheduling_control", "0", "1000000"), row("timer_arm", "1000000", "2000000"), row("quiet_wait", "2000000", "3000000")},
		"execution_ref":          map[string]any{"owner": owner, "window": "execution", "start_ns": json.Number("3000000"), "end_ns": json.Number("14000000")},
		"post_execution_control": map[string]any{"start_ns": json.Number("14000000"), "end_ns": json.Number("15000000")},
	}
	return background, owner, editProbeWindow{WallNS: 11_000_000, SumNS: 11_000_000, WithinFivePercent: true}
}

func TestSharedBackgroundReferencesExecutionExactlyOnce(t *testing.T) {
	background, owner, execution := sharedBackgroundFixture()
	if got, err := checkEditProbeSharedBackground(background, owner, execution); err != nil || got.WallNS != 15_000_000 || got.SumNS != got.WallNS {
		t.Fatalf("explicit local/E-reference/post-control partition rejected: %+v %v", got, err)
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing actual post control", func(v map[string]any) { delete(v, "post_execution_control") }},
		{"whole timer closure is not C1", func(v map[string]any) { v["end_boundary"] = "outer_timer_work_done" }},
		{"omitted excluded work", func(v map[string]any) { v["excluded_work"] = []any{"owner_proof_projection"} }},
		{"quiet gap", func(v map[string]any) {
			v["local_intervals"].([]any)[2].(map[string]any)["start_ns"] = json.Number("2000001")
		}},
		{"timer arm overlap", func(v map[string]any) {
			v["local_intervals"].([]any)[1].(map[string]any)["start_ns"] = json.Number("0")
		}},
		{"wrong E owner", func(v map[string]any) {
			v["execution_ref"].(map[string]any)["owner"] = map[string]any{"test_owner": "another execution"}
		}},
		{"copied scalar is not typed E reference", func(v map[string]any) { v["execution_ref"] = json.Number("11") }},
		{"E wall mismatch", func(v map[string]any) { v["execution_ref"].(map[string]any)["end_ns"] = json.Number("13000000") }},
		{"post control gap", func(v map[string]any) {
			v["post_execution_control"].(map[string]any)["start_ns"] = json.Number("14000001")
		}},
		{"post control does not end at C1", func(v map[string]any) {
			v["post_execution_control"].(map[string]any)["end_ns"] = json.Number("14999999")
		}},
		{"extra nested E phases", func(v map[string]any) { v["phases_ms"] = map[string]any{"resolve": json.Number("1")} }},
		{"extra local E interval", func(v map[string]any) {
			v["local_intervals"] = append(v["local_intervals"].([]any), map[string]any{"phase": "execution", "start_ns": json.Number("3000000"), "end_ns": json.Number("14000000")})
		}},
		{"producer partial", func(v map[string]any) { v["observed_partition_complete"] = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			background, owner, execution := sharedBackgroundFixture()
			tc.change(background)
			if got, err := checkEditProbeSharedBackground(background, owner, execution); err == nil {
				t.Fatalf("invalid shared B received numeric credit: %+v", got)
			}
		})
	}
}
