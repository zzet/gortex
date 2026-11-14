package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func editConsumerTestFixture(t *testing.T) (Corpus, editProbePlan) {
	t.Helper()
	c := editProbeCorpus()
	c.Symbol, c.Query = c.File+"::helper", "helper"
	c.EditAnchor = "func helper() int { return 1 }"
	c.EditTemplate = "\nfunc {{marker}}() int { return helper() }\n"
	c.ProposedSignature = "func helper(probe int) int"
	p, err := planEditProbe(c, runOptions{Seed: 1, EditCount: 1, Editors: 1, EditConsumerProbes: true})
	if err != nil {
		t.Fatal(err)
	}
	return c, p
}

func TestEditConsumerBaselineRequiresObservedMembership(t *testing.T) {
	for _, variant := range []string{"actual", "empty", "foreign", "pending", "truncated", "error", "malformed"} {
		t.Run(variant, func(t *testing.T) {
			c, plan := editConsumerTestFixture(t)
			candidate := map[string]any{"id": "owned/" + c.Symbol, "name": "helper", "file_path": c.File, "line": 2, "kind": "function"}
			body := map[string]any{"dead_code": []any{candidate}, "total": 1, "truncated": false}
			switch variant {
			case "empty":
				body["dead_code"], body["total"] = []any{}, 0
			case "foreign":
				candidate["id"] = "other.go::helper"
			case "pending":
				body["graph_freshness"], body["complete"] = "pending", false
			case "truncated":
				body["truncated"] = true
			case "malformed":
				body["total"] = "1"
			}
			var calls atomic.Int32
			socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
				calls.Add(1)
				if frame.Params["name"] != "analyze" {
					t.Error("baseline dispatched an edit or unrelated tool")
				}
				if variant == "error" {
					return nil, &rpcError{Code: -32000, Message: "unavailable"}
				}
				return stubContent(body), nil
			})
			client, err := openMCP(t.Context(), socket, "")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			sink := newEditProbeHeldSink(t)
			sink.unblock()
			s := startHeldEditProbeSpool(t, sink)
			s.step, s.record, s.client = plan.Steps[0], newEditProbeRecord(c, plan.Steps[0]), client
			r := &editProbeRuntime{plan: plan, states: []*editProbeState{s}, options: runOptions{Deadline: time.Second}, p: &privateRun{Repos: []privateRepo{{Corpus: c, Prefix: "owned"}}}, phase: &Phase{EditProbe: newEditProbeReport(plan, c, true)}}
			err = r.admitConsumerBaseline(t.Context())
			if (err == nil) != (variant == "actual") {
				t.Fatalf("baseline eligibility %s: %v", variant, err)
			}
			if (r.phase.EditProbe.ConsumerBaseline != nil) != (variant == "actual") {
				t.Fatal("unobserved subject gained a baseline witness")
			}
			if calls.Load() != 1 {
				t.Fatalf("baseline made %d calls, want one bounded read", calls.Load())
			}
			s.finishRaw()
			sink.mu.Lock()
			retained := sink.data.String()
			sink.mu.Unlock()
			if !json.Valid([]byte(retained)) {
				t.Fatal("exact single baseline response was not retained in the raw sink")
			}
		})
	}
}

func TestEditConsumerObservationsDoNotBecomeVisibilityOracles(t *testing.T) {
	for _, variant := range []string{"pending", "actual_subject", "stale_clean", "text_only", "call_error", "pre_ack"} {
		t.Run(variant, func(t *testing.T) {
			c, plan := editConsumerTestFixture(t)
			step := plan.Steps[0]
			sink := newEditProbeHeldSink(t)
			sink.unblock()
			s := startHeldEditProbeSpool(t, sink)
			s.step, s.record = step, newEditProbeRecord(c, step)
			before := copyJSONMetricMap(s.record.FirstCorrect)
			origin := time.Now()
			s.started, s.acknowledged = origin, origin.Add(time.Millisecond)
			body := map[string]any{"clean": true, "checked_callers": json.Number("0"), "violations": []any{}}
			switch variant {
			case "pending":
				body["graph_freshness"], body["complete"], body["freshness_scope"] = "pending", false, "relations"
				body["pending_files"], body["pending_file_count"] = []any{c.File}, json.Number("1")
			case "actual_subject":
				body["clean"], body["checked_callers"] = false, json.Number("1")
				body["violations"] = []any{map[string]any{"symbol_id": c.File + "::" + step.Marker, "file_path": c.File, "line": json.Number("3"), "kind": "caller_mismatch"}}
			case "text_only":
				body = map[string]any{"summary": "clean"}
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			identity := wireCallIdentity{WriteStarted: origin.Add(2 * time.Millisecond), Finished: origin.Add(3 * time.Millisecond)}
			if variant == "pre_ack" {
				identity.WriteStarted = origin.Add(time.Microsecond)
			}
			var callErr error
			if variant == "call_error" {
				callErr = errors.New("tool failed")
			}
			r := &editProbeRuntime{origin: origin, plan: plan, p: &privateRun{Repos: []privateRepo{{Corpus: c}}}, phase: &Phase{EditProbe: newEditProbeReport(plan, c, true)}}
			oracle := editConsumerOracles(c)[1]
			if err := r.observeProbe(s, oracle, identity.WriteStarted, outputWire(string(raw), nil), identity, callErr, true); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, s.record.FirstCorrect) || len(s.record.FirstCorrect) != 7 {
				t.Fatal("consumer observation changed the seven first-correct gates")
			}
			row := s.record.Consumers["consumer_verify"]
			want := map[string]string{"pending": "pending", "actual_subject": "subject_observed", "stale_clean": "silent_stale", "text_only": "malformed", "call_error": "error", "pre_ack": "missing"}[variant]
			if row.State != want {
				t.Fatalf("state=%s, want %s", row.State, want)
			}
			if (row.SilentStale.Value != nil) != (variant == "pending" || variant == "actual_subject" || variant == "stale_clean") {
				t.Fatal("missing/failed/pre-ACK observations acquired a numeric zero")
			}
			if variant == "stale_clean" && *row.SilentStale.Value != 1 {
				t.Fatal("unlabelled stale clean answer was lost")
			}
		})
	}
}

func copyJSONMetricMap(in map[string]Metric) map[string]Metric {
	out := make(map[string]Metric, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func TestEditConsumerCompletionRetainsMissingAndUnsupportedDenominators(t *testing.T) {
	c, plan := editConsumerTestFixture(t)
	record := newEditProbeRecord(c, plan.Steps[0])
	if editConsumersFinished(record.Consumers) || !editConsumersFailure(record.Consumers) {
		t.Fatal("missing offered consumers became a complete denominator")
	}
	s := &editProbeState{record: record}
	for _, name := range editConsumerNames {
		s.markUnsupportedConsumer(name)
	}
	if !editConsumersFinished(s.record.Consumers) || !editConsumersFailure(s.record.Consumers) {
		t.Fatal("unsupported must stop waiting but remain incomplete, never PASS")
	}
	for _, row := range s.record.Consumers {
		if row.SilentStale.Value != nil || row.Attempts != 0 {
			t.Fatal("compact fallback supplied fabricated coverage")
		}
	}
	if !editConsumersFinished(nil) || editConsumersFailure(nil) {
		t.Fatal("default completion changed without opt-in")
	}
}

func TestEditConsumerFreshDeadCodeTransitionRequiresBaseline(t *testing.T) {
	c, plan := editConsumerTestFixture(t)
	body := map[string]any{"dead_code": []any{}, "total": json.Number("0"), "truncated": false}
	r := &editProbeRuntime{plan: plan, phase: &Phase{EditProbe: &editProbeReport{}}}
	if r.consumerVerdict("consumer_dead_code", body, c, plan.Steps[0]) != "malformed" {
		t.Fatal("empty fresh list alone became membership proof")
	}
	r.phase.EditProbe.ConsumerBaseline = &editConsumerBaseline{State: "OBSERVED_DEAD_BEFORE_EDIT", Symbol: c.Symbol}
	if r.consumerVerdict("consumer_dead_code", body, c, plan.Steps[0]) != "fresh_membership_transition" {
		t.Fatal("observed baseline-to-fresh membership transition was lost")
	}
}

func TestEditConsumerPendingRequiresTypedAnalysisAndOwnedScope(t *testing.T) {
	for _, variant := range []string{"actual", "text_only", "foreign_file", "contradictory_complete", "fractional_count"} {
		t.Run(variant, func(t *testing.T) {
			c, plan := editConsumerTestFixture(t)
			body := map[string]any{"clean": true, "checked_callers": json.Number("0"), "violations": []any{}, "graph_freshness": "pending", "freshness_scope": "relations", "complete": false, "pending_files": []any{c.File}, "pending_file_count": json.Number("1")}
			switch variant {
			case "text_only":
				delete(body, "violations")
			case "foreign_file":
				body["pending_files"] = []any{"other.go"}
			case "contradictory_complete":
				body["complete"] = true
			case "fractional_count":
				body["pending_file_count"] = json.Number("1.5")
			}
			r := &editProbeRuntime{plan: plan}
			want := "malformed"
			if variant == "actual" {
				want = "pending"
			}
			if got := r.consumerVerdict("consumer_verify", body, c, plan.Steps[0]); got != want {
				t.Fatalf("pending verdict %s, want %s", got, want)
			}
		})
	}
}

func TestEditConsumerOptInPreservesOfferedEditsAndACKRequests(t *testing.T) {
	c, with := editConsumerTestFixture(t)
	without, err := planEditProbe(c, runOptions{Seed: 1, EditCount: 1, Editors: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i, actual := range with.Steps {
		expected := without.Steps[i]
		actual.ConsumerProbes = false
		if !reflect.DeepEqual(actual, expected) {
			t.Fatal("consumer opt-in changed the offered edit, deadline, arrival or JSON ACK request")
		}
	}
	if with.Count != without.Count || with.ProbePeriodNS != without.ProbePeriodNS || with.MeanNS != without.MeanNS {
		t.Fatal("consumer option changed the schedule or polling period")
	}
	paired := []string{"--build", "main=/private/main", "--build", "candidate=/private/candidate", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--edit-agents", "1", "--worktrees", "0", "--edit-consumer-probes"}
	if _, _, err := parsePaired(paired); err == nil {
		t.Fatal("standalone consumer observations silently changed coupled measurements")
	}
}
