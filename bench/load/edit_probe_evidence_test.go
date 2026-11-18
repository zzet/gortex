package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEditProbeOraclesRejectEchoAndRequireActualSourceAndEdges(t *testing.T) {
	corpus := editProbeCorpus()
	plan, err := makeEditProbePlan(corpus, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps[0]
	for oracle, collection := range map[string]string{"summary": "nodes", "editing_context": "defines", "search_visible": "results"} {
		facts := map[string]any{collection: []any{map[string]any{"id": "sample/" + corpus.File + "::" + step.Marker}}}
		if !editProbeCorrect(oracle, facts, corpus, step, plan.Callee) {
			t.Fatalf("%s rejected its producer collection", oracle)
		}
		if editProbeCorrect(oracle, map[string]any{"guidance": facts}, corpus, step, plan.Callee) {
			t.Fatalf("%s accepted nested guidance", oracle)
		}
	}
	echo := map[string]any{"query": step.Marker, "target": map[string]any{"id": "sample/" + corpus.File + "::" + step.Marker}}
	for _, name := range []string{"summary", "editing_context", "search_visible", "text", "callers", "source"} {
		if editProbeCorrect(name, echo, corpus, step, plan.Callee) {
			t.Fatalf("%s accepted query echo", name)
		}
	}
	source := strings.ReplaceAll(corpus.EditTemplate, "{{marker}}", step.Marker)
	if !editProbeCorrect("source", map[string]any{"name": step.Marker, "source": source}, corpus, step, plan.Callee) {
		t.Fatal("actual source/call rejected")
	}
	edge := map[string]any{"edges": []any{map[string]any{"from": "sample/" + corpus.File + "::" + step.Marker, "to": "sample/" + corpus.Symbol, "kind": "calls"}}}
	if !editProbeCorrect("callers", edge, corpus, step, plan.Callee) {
		t.Fatal("actual intrafile call rejected")
	}
	edge["edges"].([]any)[0].(map[string]any)["to"] = "ext::" + plan.Callee
	if editProbeCorrect("callers", edge, corpus, step, plan.Callee) {
		t.Fatal("external/name-only relation became source proof")
	}
}

func TestEditProbeWindowRejectsMissingFractionalNanosecondsAndOverlap(t *testing.T) {
	check, err := checkEditProbeWindow(json.Number("0.000003"), map[string]any{"a": json.Number("0.000001"), "b": json.Number("0.000002")})
	if err != nil || check.WallNS != 3 || check.ResidualNS != 0 {
		t.Fatalf("exact fractional-ms check%+v %v", check, err)
	}
	for _, value := range []any{nil, json.Number("0.0000001"), json.Number("-1"), json.Number("999999999999999999999")} {
		if _, err := editProbeNS(value); err == nil {
			t.Fatalf("invalid scalar accepted%v", value)
		}
	}
	if _, err := checkEditProbeWindow(json.Number("1"), map[string]any{"a": json.Number("2")}); err == nil {
		t.Fatal("overlapping/excess scalar sum accepted")
	}
}

func editProbeAccountingFixture() map[string]any {
	ackPhases, ackStates := map[string]any{}, map[string]any{}
	for _, name := range []string{"caller_result", "disk_write", "edit_control", "middleware", "parse", "publish", "reindex_wait", "reply_prepare"} {
		ackPhases[name], ackStates[name] = json.Number("1"), "measured"
	}
	p, b, pStates := map[string]any{}, map[string]any{}, map[string]any{}
	for _, name := range []string{"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "attempt_setup", "patch_inspection", "executor_control", "attempt_finalize", "cohort_admission", "lane_wait"} {
		p[name], b[name] = json.Number("1"), json.Number("1")
		pStates[name] = "measured"
	}
	for _, name := range requestedBackgroundLapNamesForProbe {
		b[name] = json.Number("1")
	}
	return map[string]any{
		"receipt": "receipt-a", "reindex_generation": json.Number("1"), "applied_generation": json.Number("2"),
		"acknowledgement":       map[string]any{"rpc_id": "rpc-a", "disjoint_observed_intervals": true, "caller_response_ready_ms": json.Number("8"), "observed_execution_ms": ackPhases, "phase_states": ackStates},
		"publication_phases":    map[string]any{"key": "original"},
		"coalesced_publication": map[string]any{"requested_generation": json.Number("1"), "applied_generation": json.Number("2"), "scope": "independent_physical_winner_serving_coalesced_receipt", "publication_phases": map[string]any{"ticket": json.Number("2"), "source": "canonical_coalesced_attempt", "key": "winner"}, "timings": map[string]any{"phase_timing": map[string]any{"patch_wall_ms": json.Number("13"), "patch_accounting": map[string]any{"observed_partition_complete": true, "attempt_ticket": json.Number("2"), "attempt_retry": json.Number("0"), "phases_ms": p, "phase_states": pStates}}}},
		"requested_background":  map[string]any{"requested_generation": json.Number("1"), "applied_generation": json.Number("2"), "observed_partition_complete": true, "scope": "accepted_scheduling_call_entry_to_serving_attempt_return", "coverage": "one_canceled_timer_one_native_successor", "sum_state": "partial", "background_wall_ms": json.Number("20"), "phases_ms": b},
	}
}

func TestEditProbeAccountingRejectsMissingApplicableACKAndWrongAttempt(t *testing.T) {
	fixture := editProbeAccountingFixture
	if got := checkEditProbeAccounting(fixture(), "receipt-a"); got.State != "observed_single_window_checks_pass" {
		t.Fatalf("valid fixture: %+v", got)
	}
	for _, name := range []string{"missing_ack", "wrong_retry", "wrong_owner", "unmeasured_physical"} {
		t.Run(name, func(t *testing.T) {
			body := fixture()
			if name == "missing_ack" {
				delete(body["acknowledgement"].(map[string]any)["observed_execution_ms"].(map[string]any), "parse")
			} else {
				p := body["coalesced_publication"].(map[string]any)["timings"].(map[string]any)["phase_timing"].(map[string]any)["patch_accounting"].(map[string]any)
				switch name {
				case "unmeasured_physical":
					p["phase_states"].(map[string]any)["apply"] = "not_observed"
				case "wrong_retry":
					p["attempt_retry"] = json.Number("1")
				default:
					p["attempt_ticket"] = json.Number("9")
				}
			}
			if got := checkEditProbeAccounting(body, "receipt-a"); got.State == "observed_single_window_checks_pass" {
				t.Fatalf("invalid fixture accepted: %+v", got)
			}
		})
	}
}

func TestEditProbeFreshnessDoesNotReadGuidanceExamples(t *testing.T) {
	fresh := map[string]any{"freshness": map[string]any{"graph_freshness": "fresh"}}
	if !editProbeClaimsFresh(fresh) || editProbeClaimsFresh(map[string]any{"guidance": fresh}) {
		t.Fatal("freshness rider confused with nested example")
	}
}

func TestEditProbeWaitsForLatePhysicalReturnButStopsImmutableMissingLaps(t *testing.T) {
	fixture := func(partial bool) map[string]any {
		body := editProbeAccountingFixture()
		winner := body["coalesced_publication"].(map[string]any)
		pub := winner["publication_phases"].(map[string]any)
		pub["clock"], pub["terminal"] = "monotonic", true
		physical := winner["timings"].(map[string]any)["phase_timing"].(map[string]any)
		physical["clock"] = "monotonic"
		accounting := physical["patch_accounting"].(map[string]any)
		accounting["scope"] = "accepted_winning_physical_attempt"
		if partial {
			accounting["observed_partition_complete"], accounting["sum_state"] = false, "missing_laps"
			accounting["phases_ms"].(map[string]any)["apply"] = nil
			accounting["phase_states"].(map[string]any)["apply"] = "not_observed"
			physical["patch_wall_ms"] = json.Number("12")
		}
		delete(body, "requested_background")
		return body
	}
	early := fixture(true)
	early["coalesced_publication"].(map[string]any)["timings"].(map[string]any)["phase_timing"].(map[string]any)["patch_wall_ms"] = nil
	earlyView := checkEditProbeAccounting(early, "receipt-a")
	if earlyView.State != "unavailable" || editProbeFinished(true, true, true, earlyView) {
		t.Fatal("receipt terminality stopped waiting before late P")
	}
	lateView := checkEditProbeAccounting(fixture(true), "receipt-a")
	if lateView.State != "terminal_partial_physical" || len(lateView.Reasons) == 0 || !editProbeFinished(true, true, true, lateView) {
		t.Fatalf("immutable missing deep laps did not terminate failed coverage: %+v", lateView)
	}
	if editProbeFinished(false, true, true, lateView) || editProbeFinished(true, false, true, lateView) {
		t.Fatal("missing graph/read evidence was discarded")
	}
	if earlyView.State != "unavailable" || checkEditProbeAccounting(early, "receipt-a").State != "unavailable" {
		t.Fatal("later observation changed an earlier snapshot")
	}
	completeWithoutB := checkEditProbeAccounting(fixture(false), "receipt-a")
	if completeWithoutB.State != "unavailable" || editProbeFinished(true, true, true, completeWithoutB) {
		t.Fatal("complete P stopped before late B publication")
	}
	for _, bad := range []string{"owner", "clock", "terminal", "wall", "retry"} {
		t.Run(bad, func(t *testing.T) {
			body := fixture(true)
			winner := body["coalesced_publication"].(map[string]any)
			pub := winner["publication_phases"].(map[string]any)
			physical := winner["timings"].(map[string]any)["phase_timing"].(map[string]any)
			switch bad {
			case "owner":
				pub["ticket"] = json.Number("9")
			case "clock":
				pub["clock"] = "unavailable"
			case "terminal":
				pub["terminal"] = false
			case "wall":
				physical["patch_wall_ms"] = json.Number("-1")
			case "retry":
				delete(physical["patch_accounting"].(map[string]any), "attempt_retry")
			}
			if got := checkEditProbeAccounting(body, "receipt-a"); got.State == "terminal_partial_physical" {
				t.Fatalf("unproven late-window identity stopped waiting: %+v", got)
			}
		})
	}
}

func TestEditProbeObservedDuplicateWaitsForPublisherCoverage(t *testing.T) {
	fixture := func() map[string]any {
		body := editProbeAccountingFixture()
		winner := body["coalesced_publication"].(map[string]any)
		pub := winner["publication_phases"].(map[string]any)
		pub["clock"], pub["terminal"] = "monotonic", true
		physical := winner["timings"].(map[string]any)["phase_timing"].(map[string]any)
		physical["clock"], physical["patch_wall_ms"] = "monotonic", json.Number("5")
		a := physical["patch_accounting"].(map[string]any)
		a["scope"], a["sum_state"], a["execution_disposition"] = "accepted_winning_physical_attempt", "partial", "native_mtime_duplicate"
		phases, states := a["phases_ms"].(map[string]any), a["phase_states"].(map[string]any)
		for _, name := range []string{"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "patch_analysis", "executor_control"} {
			phases[name], states[name] = nil, "not_applicable"
		}
		delete(body, "requested_background")
		return body
	}
	body := fixture()
	got := checkEditProbeAccounting(body, "receipt-a")
	if got.State != "unavailable" || len(got.Reasons) == 0 || editProbeFinished(true, true, true, got) {
		t.Fatalf("observed no-op cannot prove late publisher evidence absent: %+v", got)
	}
	for _, bad := range []string{"early", "disposition", "deep_value", "deep_state", "control", "gap", "retry", "owner", "receipt"} {
		t.Run(bad, func(t *testing.T) {
			body := fixture()
			winner := body["coalesced_publication"].(map[string]any)
			physical := winner["timings"].(map[string]any)["phase_timing"].(map[string]any)
			a := physical["patch_accounting"].(map[string]any)
			switch bad {
			case "early":
				physical["patch_wall_ms"] = nil
			case "disposition":
				delete(a, "execution_disposition")
			case "deep_value":
				a["phases_ms"].(map[string]any)["apply"] = json.Number("0")
			case "deep_state":
				a["phase_states"].(map[string]any)["apply"] = "not_observed"
			case "control":
				a["phase_states"].(map[string]any)["attempt_finalize"] = "not_observed"
			case "gap":
				physical["patch_wall_ms"] = json.Number("6")
			case "retry":
				a["attempt_retry"] = json.Number("1")
			case "owner":
				winner["applied_generation"] = json.Number("3")
			case "receipt":
				body["receipt"] = "foreign"
			}
			got := checkEditProbeAccounting(body, "receipt-a")
			if got.State == "terminal_partial_physical" || got.State == "observed_single_window_checks_pass" || editProbeFinished(true, true, true, got) {
				t.Fatalf("unproven no-op evidence terminated collection: %+v", got)
			}
		})
	}
}
