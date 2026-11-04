package main

import (
	"encoding/json"
	"strings"
	"testing"
 "time"
)

func readFreshnessReportFixture(code string) map[string]any {
	raw := syntheticReport()
	raw["workload"].(map[string]any)["agents"] = 16
	p := raw["phases"].([]any)[0].(map[string]any)
	identity := func(retry int, outcome string, status int, logical float64) map[string]any {
		return map[string]any{"cwd": "/owned/private", "request_sha256": strings.Repeat("c", 64), "agent": 3, "step": 0, "slot": 1, "retry": retry, "duration_ms": 2, "logical_ms": logical, "started": time.Date(2026,11,4,12,0,0,0,time.UTC).Add(time.Duration(logical-2)*time.Millisecond), "tool": "read.summary", "role": "workload", "outcome": outcome, "code": status, "error": ""}
	}
	first := identity(0, "retryable", -32000, 2)
	first["error"] = code + ": selected read unavailable"
	first["error_data"] = map[string]any{"error_code": code, "retriable": true, "retry_scope": "read_only", "delivery_unknown": false}
	last := identity(1, "success", 0, 50)
	p["attempts"] = []any{first, last}
	p["tools"] = map[string]any{"read.summary": map[string]any{"successes": 1, "retryable_refusals": 1, "errors": map[string]any{}, "phase_censored": 0, "product_expired": 0, "latency_ms": map[string]any{"count": 1, "p95": map[string]any{"value": 50, "unit": "ms", "reason": ""}}}}
	return raw
}
func validateFreshnessFixture(t *testing.T, raw map[string]any) error {
	t.Helper()
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	r, err := decodeReport(data)
	if err != nil {
		return err
	}
	c := inputs{CandidateRevision: strings.Repeat("a", 40), Hosts: map[string]string{"reference_linux": "synthetic-host"}}
	return validateReport(c, runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "load-16"}, r)
}
func TestReadFreshnessCheckerRetainsRefusalAndTerminalLogicalSuccess(t *testing.T) {
	for _, code := range []string{"projection_superseded", "file_view_pending"} {
		t.Run(code, func(t *testing.T) {
			raw := readFreshnessReportFixture(code)
			if err := validateFreshnessFixture(t, raw); err != nil {
				t.Fatal(err)
			}
			p := raw["phases"].([]any)[0].(map[string]any)
			first := p["attempts"].([]any)[0].(map[string]any)
			if first["outcome"] != "retryable" || first["error"] == "" || p["tools"].(map[string]any)["read.summary"].(map[string]any)["retryable_refusals"] != 1 {
				t.Fatal("validation erased original refused response/count")
			}
		})
	}
}
func TestReadFreshnessCheckerRejectsMissingOrWeakenedEvidence(t *testing.T) {
	for _, name := range []string{"generic_error", "message_only", "unknown_code", "false_metadata", "missing_delivery", "unknown_delivery", "missing_first", "missing_terminal", "request_changed", "duplicate_retry", "cost_reset", "deadline_exceeded", "cap_exceeded", "refusal_count_erased", "refusal_count_missing", "success_count_inflated", "probe", "write", "terminal_error"} {
		t.Run(name, func(t *testing.T) {
			raw := readFreshnessReportFixture("projection_superseded")
			p := raw["phases"].([]any)[0].(map[string]any)
			rows := p["attempts"].([]any)
			first := rows[0].(map[string]any)
			last := rows[1].(map[string]any)
			data := first["error_data"].(map[string]any)
			m := p["tools"].(map[string]any)["read.summary"].(map[string]any)
			switch name {
			case "generic_error":
				first["outcome"] = "error"
			case "message_only":
				delete(first, "error_data")
			case "unknown_code":
				data["error_code"] = "unknown"
			case "false_metadata":
				data["retriable"] = false
			case "missing_delivery":
				delete(data, "delivery_unknown")
			case "unknown_delivery":
				data["delivery_unknown"] = true
			case "missing_first":
				p["attempts"] = rows[1:]
			case "missing_terminal":
				p["attempts"] = rows[:1]
			case "request_changed":
				last["request_sha256"] = strings.Repeat("d", 64)
			case "duplicate_retry":
				last["retry"] = 0
			case "cost_reset":
				last["logical_ms"] = 1
			case "deadline_exceeded":
				raw["workload"].(map[string]any)["product_deadline_seconds"] = .01
			case "cap_exceeded":
				next := func(retry int) map[string]any {
					clone := make(map[string]any)
					for k, v := range first {
						clone[k] = v
					}
					clone["retry"] = retry
					clone["logical_ms"] = float64(2 * (retry + 1))
 clone["started"] = time.Date(2026,11,4,12,0,0,0,time.UTC).Add(time.Duration(2*retry)*time.Millisecond)
					return clone
				}
				last["retry"] = 3
				p["attempts"] = []any{first, next(1), next(2), last}
				m["retryable_refusals"] = 3
			case "refusal_count_erased":
				m["retryable_refusals"] = 0
			case "refusal_count_missing":
				delete(m, "retryable_refusals")
			case "success_count_inflated":
				m["successes"] = 2
			case "probe":
				first["role"] = "probe"
				last["role"] = "probe"
			case "write":
				first["tool"] = "edit.file"
				last["tool"] = "edit.file"
			case "terminal_error":
				last["outcome"] = "error"
				last["code"] = -32000
				last["error"] = "still unavailable"
			}
			if err := validateFreshnessFixture(t, raw); err == nil {
				t.Fatal("weakened/missing original retry evidence accepted")
			}
		})
	}
}
func TestReadFreshnessCheckerLeavesLegacyBusyProbePolicyUnchanged(t *testing.T) {
	raw := readFreshnessReportFixture("projection_superseded")
	p := raw["phases"].([]any)[0].(map[string]any)
	rows := p["attempts"].([]any)
	first := rows[0].(map[string]any)
	first["outcome"] = "busy"
	first["code"] = -32002
	delete(first, "error_data")
	first["role"] = "probe"
	rows[1].(map[string]any)["role"] = "probe"
	delete(p["tools"].(map[string]any)["read.summary"].(map[string]any), "retryable_refusals")
	if err := validateFreshnessFixture(t, raw); err != nil {
		t.Fatal(err)
	}
}

// A declared refusal is evidence even when a corrupted history resets or drops
// its retry index. Sequential attempt cost cannot erase previous wire work.
func TestReadFreshnessCheckerRejectsOrphanCountersAndOmittedAttemptCost(t *testing.T) {
	for _, name := range []string{"original_removed_retry_reset", "original_removed_retry_omitted", "other_tool_orphan_counter", "previous_attempt_cost_omitted"} {
		t.Run(name, func(t *testing.T) {
			raw := readFreshnessReportFixture("projection_superseded")
			p := raw["phases"].([]any)[0].(map[string]any)
			rows := p["attempts"].([]any)
			first := rows[0].(map[string]any)
			last := rows[1].(map[string]any)
			switch name {
			case "original_removed_retry_reset":
				p["attempts"] = rows[1:]
				last["retry"] = 0
			case "original_removed_retry_omitted":
				p["attempts"] = rows[1:]
				delete(last, "retry")
			case "other_tool_orphan_counter":
				other := make(map[string]any)
				for key, value := range last {
					other[key] = value
				}
				other["tool"], other["slot"], other["retry"], other["logical_ms"] = "read.editing_context", 2, 0, 2
				p["attempts"] = append(rows, other)
				p["tools"].(map[string]any)["read.editing_context"] = map[string]any{"successes": 1, "retryable_refusals": 1, "errors": map[string]any{}, "phase_censored": 0, "product_expired": 0, "latency_ms": map[string]any{"count": 1, "p95": map[string]any{"value": 2, "unit": "ms", "reason": ""}}}
			case "previous_attempt_cost_omitted":
				first["duration_ms"], first["logical_ms"] = 100, 100
				last["duration_ms"], last["logical_ms"] = 100, 100
			}
			if err := validateFreshnessFixture(t, raw); err == nil {
				t.Fatal("orphan refusal or omitted sequential attempt cost accepted")
			}
		})
	}
}

func TestReadFreshnessCheckerKeepsInterleavedGroupsAndBusyBackoff(t *testing.T) {
	for _, name := range []string{"interleaved_groups", "busy_then_explicit_refusal"} {
		t.Run(name, func(t *testing.T) {
			raw := readFreshnessReportFixture("projection_superseded")
			p := raw["phases"].([]any)[0].(map[string]any)
			rows := p["attempts"].([]any)
			clone := func(row map[string]any) map[string]any {
				result := make(map[string]any)
				for key, value := range row {
					result[key] = value
				}
				return result
			}
			first, last := rows[0].(map[string]any), rows[1].(map[string]any)
			switch name {
			case "interleaved_groups":
				otherFirst, otherLast := clone(first), clone(last)
				otherFirst["tool"], otherLast["tool"] = "read.editing_context", "read.editing_context"
				otherFirst["slot"], otherLast["slot"] = 2, 2
				p["attempts"] = []any{first, otherFirst, last, otherLast}
				metrics := make(map[string]any)
				for key, value := range p["tools"].(map[string]any)["read.summary"].(map[string]any) {
					metrics[key] = value
				}
				p["tools"].(map[string]any)["read.editing_context"] = metrics
			case "busy_then_explicit_refusal":
				busy := clone(first)
				busy["outcome"], busy["code"], busy["error"] = "busy", -32002, "busy"
				delete(busy, "error_data")
				first["retry"], first["logical_ms"] = 1, 10
 first["started"] = time.Date(2026,11,4,12,0,0,8*int(time.Millisecond),time.UTC)
				last["retry"] = 2
				p["attempts"] = []any{busy, first, last}
			}
			if err := validateFreshnessFixture(t, raw); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadFreshnessCheckerRejectsDroppedBackoffAndMissingTimestamps(t *testing.T) {
 for _, name := range []string{"backoff_omitted", "first_started_missing", "last_started_missing", "started_moves_backwards"} {
  t.Run(name,func(t *testing.T){
   raw:=readFreshnessReportFixture("projection_superseded")
   rows:=raw["phases"].([]any)[0].(map[string]any)["attempts"].([]any)
   first,last:=rows[0].(map[string]any),rows[1].(map[string]any)
   switch name {
   case "backoff_omitted": last["logical_ms"]=4 // two2ms attempts but actual retained start gap48ms
   case "first_started_missing": delete(first,"started")
   case "last_started_missing": delete(last,"started")
   case "started_moves_backwards": last["started"]=first["started"].(time.Time).Add(-time.Millisecond)
   }
   if err:=validateFreshnessFixture(t,raw);err==nil{t.Fatal("missing original timestamps or omitted backoff cost accepted")}
  })
 }
}
