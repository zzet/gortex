package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestEditProbeStaleClaimsRequireSuccessfulObservableAnswer(t *testing.T) {
	corpus := editProbeCorpus()
	corpus.EditKind = "add_intrafile_call"
	plan, err := makeEditProbePlan(corpus, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	plan.GateModes = true
	step := plan.Steps[0]
	step.ProbeMode = "json"
	marker := step.Marker
	callers := func(name string) map[string]any {
		return map[string]any{
			"freshness": "fresh",
			"edges": []any{map[string]any{
				"from": corpus.File + "::" + name,
				"to":   corpus.Symbol, "kind": "calls",
			}},
		}
	}
	summary := func(id, name string) map[string]any {
		return map[string]any{
			"freshness": "fresh", "_truncated_by_budget": true,
			"_original_count_nodes": 2, "_max_returned_nodes": 1,
			"nodes": []any{map[string]any{"id": id, "name": name}},
		}
	}
	missingCensus := summary(corpus.File+"::Other", "Other")
	delete(missingCensus, "_original_count_nodes")
	mismatchedCensus := summary(corpus.File+"::Other", "Other")
	mismatchedCensus["_max_returned_nodes"] = 0
	wrongSummaryPath := summary(corpus.File+"::Other", "Other")
	wrongSummaryPath["file_path"] = "different.go"
	wrongNodePath := summary(corpus.File+"::Other", "Other")
	wrongNodePath["nodes"].([]any)[0].(map[string]any)["file_path"] = "different.go"
	wrongNodePathOnly := summary("", "Other")
	wrongNodePathOnly["nodes"].([]any)[0].(map[string]any)["path"] = "different.go"
	wrongRange := map[string]any{
		"freshness": "fresh", "_truncated_by_budget": true,
		"id": corpus.File + "::" + marker, "name": marker,
		"file_path": corpus.File, "from_line": 99, "start_line": 99,
		"source": "func Other() int { return Example() }",
	}
	wrongName := map[string]any{
		"freshness": "fresh", "_truncated_by_budget": true,
		"id": corpus.File + "::" + marker, "name": "Other",
		"file_path": corpus.File,
		"source":    "func " + marker + "() int { return Example() }",
	}
	wrongCallee := map[string]any{
		"freshness": "fresh", "_truncated_by_budget": true,
		"edges": []any{map[string]any{
			"from": corpus.File + "::" + marker,
			"to":   corpus.File + "::Other", "kind": "calls",
		}},
	}
	cases := []struct {
		name            string
		mode            string
		oracle          string
		body            map[string]any
		err             error
		wantStale       bool
		wantCorrect     bool
		wantUnavailable bool
		preACK          bool
	}{
		{name: "correct", oracle: "callers", body: callers(marker), wantCorrect: true},
		{name: "canceled_correct_bytes", oracle: "callers", body: callers(marker), err: context.Canceled},
		{name: "deadline_correct_bytes", oracle: "callers", body: callers(marker), err: context.DeadlineExceeded},
		{name: "canceled_wrong_bytes", oracle: "callers", body: callers("Other"), err: context.Canceled},
		{name: "successful_wrong_edge", oracle: "callers", body: callers("Other"), wantStale: true},
		{name: "clipped_visible_wrong_callee", oracle: "callers", body: wrongCallee, wantStale: true},
		{name: "summary_subject_omitted", oracle: "summary", body: summary(corpus.File+"::Other", "Other"), wantUnavailable: true},
		{name: "cli_summary_subject_omitted", mode: "cli", oracle: "summary", body: summary(corpus.File+"::Other", "Other"), wantUnavailable: true},
		{name: "summary_omitted_before_ACK", oracle: "summary", body: summary(corpus.File+"::Other", "Other"), preACK: true},
		{name: "summary_visible_correct_subject", oracle: "summary", body: summary(corpus.File+"::"+marker, marker), wantCorrect: true},
		{name: "summary_visible_wrong_file", oracle: "summary", body: summary("different.go::"+marker, marker), wantStale: true},
		{name: "cli_summary_visible_wrong_file", mode: "cli", oracle: "summary", body: summary("different.go::"+marker, marker), wantStale: true},
		{name: "clipping_without_omission_census", oracle: "summary", body: missingCensus, wantStale: true},
		{name: "clipping_with_wrong_row_census", oracle: "summary", body: mismatchedCensus, wantStale: true},
		{name: "clipped_wrong_summary_path", oracle: "summary", body: wrongSummaryPath, wantStale: true},
		{name: "clipped_foreign_node_ID_without_subject", oracle: "summary", body: summary("different.go::Other", "Other"), wantStale: true},
		{name: "clipped_foreign_node_path_without_subject", oracle: "summary", body: wrongNodePath, wantStale: true},
		{name: "clipped_foreign_node_path_only_without_subject", oracle: "summary", body: wrongNodePathOnly, wantStale: true},
		{name: "visible_wrong_source_range", oracle: "source", body: wrongRange, wantStale: true},
		{name: "visible_wrong_source_name", oracle: "source", body: wrongName, wantStale: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := newEditProbeHeldSink(t)
			sink.unblock()
			state := startHeldEditProbeSpool(t, sink)
			caseStep := step
			if tc.mode != "" {
				caseStep.ProbeMode = tc.mode
			}
			state.step, state.record = caseStep, newEditProbeRecord(corpus, caseStep)
			state.record.Errors = nil // prepareEditProbe clears the undispatched placeholder.
			origin := time.Now()
			state.started, state.acknowledged = origin.Add(time.Millisecond), origin.Add(2*time.Millisecond)
			run := &editProbeRuntime{origin: origin, plan: plan, p: &privateRun{Repos: []privateRepo{{Corpus: corpus}}}}
			var oracle editProbeOracle
			for _, candidate := range editProbeOracles(corpus, step) {
				if candidate.name == tc.oracle {
					oracle = candidate
				}
			}
			if oracle.name == "" {
				t.Fatal("test oracle is unavailable")
			}
			encoded, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			identity := wireCallIdentity{WriteStarted: origin.Add(3 * time.Millisecond), Finished: origin.Add(4 * time.Millisecond)}
			if tc.preACK {
				identity.WriteStarted, identity.Finished = origin.Add(1500*time.Microsecond), origin.Add(1800*time.Microsecond)
			}
			payload := outputWire(string(encoded), nil)
			if tc.mode == "cli" {
				payload = encoded
			}
			if err := run.observeProbe(state, oracle, identity.WriteStarted, payload, identity, tc.err, false); err != nil {
				t.Fatal(err)
			}
			if state.record.StaleClaimedFresh != tc.wantStale {
				t.Fatalf("stale=%v, want %v", state.record.StaleClaimedFresh, tc.wantStale)
			}
			if correct := state.record.FirstCorrect[tc.oracle].Value != nil; correct != tc.wantCorrect {
				t.Fatalf("correct-oracle credit=%v, want %v", correct, tc.wantCorrect)
			}
			wantErrors := 0
			if tc.err != nil {
				wantErrors = 1
			}
			if state.record.ProbeErrors != wantErrors {
				t.Fatalf("probe errors=%d, want %d", state.record.ProbeErrors, wantErrors)
			}
			if unavailable := len(state.record.Errors) != 0; unavailable != tc.wantUnavailable {
				t.Fatalf("unavailable evidence=%v, want %v: %v", unavailable, tc.wantUnavailable, state.record.Errors)
			}
		})
	}
}
