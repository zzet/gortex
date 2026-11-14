package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEditConsumerCompactCensusAndScope(t *testing.T) {
	const dead = "dead_code_v1\t0\t0\tfalse\n"
	const verify = "verify_v1\tfalse\t1\t0\t1\t0\nviolation\t\"caller_mismatch\"\t\"main.go::Caller\"\t\"Caller\"\t\"main.go\"\t4\t\"missing parameter\"\n"
	const pending = "derived: pending (1 files)\n" + dead + "pending_file \"main.go\"\n"
	for name, tc := range map[string]struct {
		operation, text string
		valid           bool
	}{
		"observed_empty_census":                {"analyze.dead_code", dead, true},
		"actual_violation":                     {"change.verify", verify, true},
		"actual_pending_scope":                 {"analyze.dead_code", pending, true},
		"missing_census":                       {"analyze.dead_code", "no dead code found\n", false},
		"wrong_count":                          {"analyze.dead_code", strings.Replace(dead, "\t0\t0\t", "\t1\t0\t", 1), false},
		"truncated_census":                     {"analyze.dead_code", strings.Replace(dead, "false", "true", 1), false},
		"duplicate_header":                     {"analyze.dead_code", dead + dead, false},
		"negative_count":                       {"analyze.dead_code", "dead_code_v1\t-1\t0\tfalse\n", false},
		"overflow_count":                       {"analyze.dead_code", "dead_code_v1\t18446744073709551616\t0\tfalse\n", false},
		"unknown_record":                       {"analyze.dead_code", dead + "other\t0\n", false},
		"wrong_operation":                      {"change.verify", dead, false},
		"missing_newline":                      {"analyze.dead_code", strings.TrimSuffix(dead, "\n"), false},
		"missing_scope_path":                   {"analyze.dead_code", "derived: pending (1 files)\n" + dead, false},
		"extra_scope_path":                     {"analyze.dead_code", pending + "pending_file \"other.go\"\n", false},
		"duplicate_scope_path":                 {"analyze.dead_code", pending + "pending_file \"main.go\"\n", false},
		"path_without_label":                   {"analyze.dead_code", dead + "pending_file \"main.go\"\n", false},
		"wrong_scope":                          {"analyze.dead_code", strings.Replace(pending, "derived:", "relations:", 1), false},
		"fresh_with_pending_path":              {"analyze.dead_code", strings.Replace(pending, "pending (1", "fresh (0", 1), false},
		"clean_contradiction":                  {"change.verify", strings.Replace(verify, "false", "true", 1), false},
		"omitted_violation":                    {"change.verify", strings.SplitAfter(verify, "\n")[0], false},
		"malformed_quote":                      {"change.verify", strings.Replace(verify, "\"Caller\"", "Caller", 1), false},
		"tool_error":                           {"change.verify", "verify_v1\ttrue\t0\t0\t0\t1\nerror\t\"unavailable symbol\"\n", false},
		"malformed_return_usage":               {"change.verify", verify + "return_usage\t\"id\"\t1\t0\t\"bound\"\n", false},
		"duplicate_identity_across_partitions": {"analyze.dead_code", "dead_code_v1\t1\t1\tfalse\ndead\t\"function\"\t\"main.go::helper\"\t\"helper\"\t\"main.go\"\t3\npending_unknown\t\"function\"\t\"main.go::helper\"\t\"helper\"\t\"main.go\"\t3\n", false},
		"rename_missing_status":                {"refactor.rename", "rename_v1\t\"a\"\t\"b\"\ttrue\t0\n", false},
		"rename_mutating":                      {"refactor.rename", "rename_v1\t\"a\"\t\"b\"\tfalse\t0\nrename_status\t\"applied\"\ttrue\n", false},
		"rename_wrote_during_dry_run":          {"refactor.rename", "rename_v1\t\"a\"\t\"b\"\ttrue\t0\nrename_status\t\"would_apply\"\ttrue\n", false},
		"rename_quoted_multiline":              {"refactor.rename", "rename_v1\t\"a\"\t\"b\"\ttrue\t1\nedit\t\"main.go\"\t3\t\"a()\\n\\t\\\"x\\\"\"\t\"b()\\n\\t\\\"x\\\"\"\t\"high\"\t\"reference\"\nrename_status\t\"would_apply\"\tfalse\n", true},
		"checkout_unavailable":                 {"change.detect", "detect_v1\t\"unstaged\"\t1\t0\nchanged_file\t\"main.go\"\ncomplete\tfalse\ngraph_status\t\"pending\"\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := decodeEditProbeOutput(outputWire(tc.text, nil), "compact", tc.operation)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v body=%v", tc.valid, err, body)
			}
			if tc.valid && strings.HasPrefix(tc.text, "derived:") && (body["complete"] != false || !editConsumerPending(body, "main.go", "derived")) {
				t.Fatal("actual pending scope was lost or promoted to completion")
			}
			if tc.valid && !strings.Contains(tc.text, ": pending") {
				if _, exists := body["graph_freshness"]; exists {
					t.Fatal("compact invented an idle freshness claim")
				}
			}
			if tc.valid && name == "checkout_unavailable" && editConsumerFresh(body) {
				t.Fatal("incomplete checkout was promoted to fresh")
			}
		})
	}
}

func TestEditConsumerCompactProofMatchesOperation(t *testing.T) {
	c, plan := editConsumerTestFixture(t)
	step := plan.Steps[0]
	text := fmt.Sprintf("verify_v1\tfalse\t1\t0\t1\t0\nviolation\t%q\t%q\t%q\t%q\t3\t%q\n", "caller_mismatch", c.File+"::"+step.Marker, step.Marker, c.File, "missing parameter")
	body, err := decodeEditProbeOutput(outputWire(text, nil), "compact", "change.verify")
	if err != nil {
		t.Fatal(err)
	}
	r := &editProbeRuntime{plan: plan}
	if got := r.consumerVerdict("consumer_verify", body, c, step); got != "subject_observed" {
		t.Fatal(got)
	}
	for _, proof := range []any{editCompactConsumerWire{operation: "refactor.rename"}, map[string]any{"operation": "change.verify"}} {
		body[editCompactConsumerWireKey] = proof
		if got := r.consumerVerdict("consumer_verify", body, c, step); got != "malformed" {
			t.Fatalf("foreign/untyped compact proof accepted: %s", got)
		}
	}
}

func TestEditConsumerCompactLegacyReturnUsage(t *testing.T) {
	// This is the established compact line asserted by the actual MCP
	// TestVerifyChange_ReturnUsageCompactLine compatibility regression.
	const header = "verify_v1\ttrue\t4\t0\t0\t0\n"
	const legacy = "return_usage pkg/fetch.go::Fetch call_sites:4 assigned:1 discarded:1 returned:1 unclassified:1\n"
	for name, tc := range map[string]struct {
		text  string
		valid bool
	}{
		"established_line":                   {legacy, true},
		"zero_unclassified_omitted":          {"return_usage pkg/fetch.go::Fetch call_sites:4 assigned:2 discarded:1 returned:1\n", true},
		"all_unclassified":                   {"return_usage pkg/fetch.go::Fetch call_sites:4 unclassified:4\n", true},
		"spaced_symbol_path":                 {"return_usage pkg/cache path/fetch.go::Fetch call_sites:4 assigned:4\n", true},
		"independent_checked_callers_census": {"return_usage pkg/fetch.go::Fetch call_sites:1 assigned:1\n", true},
		"missing_distribution":               {"return_usage pkg/fetch.go::Fetch call_sites:4\n", false},
		"missing_mass":                       {"return_usage pkg/fetch.go::Fetch call_sites:4 assigned:3\n", false},
		"excess_mass":                        {"return_usage pkg/fetch.go::Fetch call_sites:4 assigned:5\n", false},
		"duplicate_category":                 {"return_usage pkg/fetch.go::Fetch call_sites:4 assigned:1 assigned:3\n", false},
		"duplicate_subject":                  {legacy + "return_usage pkg/fetch.go::Fetch call_sites:1 assigned:1\n", false},
		"duplicate_call_sites":               {"return_usage pkg/fetch.go::Fetch call_sites:4 assigned:1 call_sites:3 returned:3\n", false},
		"unclassified_not_last":              {"return_usage pkg/fetch.go::Fetch call_sites:4 unclassified:1 assigned:3\n", false},
		"negative_count":                     {"return_usage pkg/fetch.go::Fetch call_sites:4 assigned:-1 unclassified:5\n", false},
		"decimal_count":                      {"return_usage pkg/fetch.go::Fetch call_sites:4 assigned:4.0\n", false},
		"noncanonical_integer":               {"return_usage pkg/fetch.go::Fetch call_sites:4 assigned:04\n", false},
		"overflow_count":                     {"return_usage pkg/fetch.go::Fetch call_sites:4 assigned:18446744073709551616\n", false},
		"missing_subject":                    {"return_usage  call_sites:4 assigned:4\n", false},
		"unknown_text":                       {"return_usage pkg/fetch.go::Fetch call_sites:4 prose\n", false},
		"replaced_tab_syntax_refused":        {"return_usage\t\"pkg/fetch.go::Fetch\"\t4\t1\t\"assigned\"\t1\t\"discarded\"\t1\t\"returned\"\t1\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := decodeEditProbeOutput(outputWire(header+tc.text, nil), "compact", "change.verify")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v body=%v", tc.valid, err, body)
			}
			if tc.valid {
				rows, ok := editConsumerRows(body, "return_usage")
				if !ok || len(rows) != 1 {
					t.Fatal("actual return-usage row was discarded")
				}
				if name == "established_line" {
					row := rows[0].(map[string]any)
					counts := row["counts"].(map[string]any)
					if row["symbol_id"] != "pkg/fetch.go::Fetch" || row["call_sites"] != json.Number("4") || row["unclassified"] != json.Number("1") || counts["assigned"] != json.Number("1") || counts["discarded"] != json.Number("1") || counts["returned"] != json.Number("1") {
						t.Fatalf("distribution changed: %v", row)
					}
				}
			}
		})
	}
}

func TestEditConsumerCompactLiteralTextAndStatus(t *testing.T) {
	// A quoted source string is data, even when it mentions the exact text
	// that the byte-budget renderer uses as a separate truncation record.
	before := "func Caller() { a(); println(\"" + editProbeCompactBudgetMarker + "\") }"
	after := strings.Replace(before, "a()", "b()", 1)
	rename := fmt.Sprintf("rename_v1\t%q\t%q\ttrue\t1\nedit\t%q\t3\t%q\t%q\t%q\t%q\nrename_status\t%q\tfalse\n", "a", "b", "main.go", before, after, "high", "reference", "would_apply")
	plainRename := strings.ReplaceAll(rename, editProbeCompactBudgetMarker, "ordinary source text")
	const noEdits = "rename_v1\t\"a\"\t\"b\"\ttrue\t0\nrename_status\t\"no_edits\"\tfalse\n"
	const detect = "detect_v1\t\"unstaged\"\t1\t0\nchanged_file\t\"main.go\"\n"
	for name, tc := range map[string]struct {
		operation, text string
		valid           bool
	}{
		"literal_marker_in_source":          {"refactor.rename", rename, true},
		"actual_budget_marker_record":       {"analyze.dead_code", "dead_code_v1\t0\t0\tfalse\n" + editProbeCompactBudgetMarker + "\n", false},
		"detect_pending_missing_incomplete": {"change.detect", detect + "graph_status\t\"pending\"\n", false},
		"detect_pending_claims_complete":    {"change.detect", detect + "complete\ttrue\ngraph_status\t\"pending\"\n", false},
		"detect_failed_missing_incomplete":  {"change.detect", detect + "graph_status\t\"failed\"\n", false},
		"rename_no_edits_with_row":          {"refactor.rename", strings.Replace(plainRename, "\"would_apply\"", "\"no_edits\"", 1), false},
		"rename_would_apply_empty":          {"refactor.rename", strings.Replace(noEdits, "\"no_edits\"", "\"would_apply\"", 1), false},
		"rename_provisional_only_empty":     {"refactor.rename", strings.Replace(noEdits, "\"no_edits\"", "\"provisional_only\"", 1), false},
		"consistent_no_edits":               {"refactor.rename", noEdits, true},
		"consistent_checkout_pending":       {"change.detect", detect + "complete\tfalse\ngraph_status\t\"pending\"\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := decodeEditProbeOutput(outputWire(tc.text, nil), "compact", tc.operation)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v body=%v", tc.valid, err, body)
			}
			if tc.valid && name == "literal_marker_in_source" && body["edits"].([]any)[0].(map[string]any)["old_text"] != before {
				t.Fatal("literal source bytes changed")
			}
		})
	}
}

func TestEditConsumerCompactSeparatesActualSessionNotes(t *testing.T) {
	// momentum.go appends this as a second content item, never inside its
	// producer payload. Reuse the existing exact/ordered auxiliary-note boundary.
	const note = "(Session note: graph read #6. Every location returned so far is real and citeable — if you are localizing or answering a question, consider writing your conclusion from what you already hold and fetching only what it still lacks. This note appears once.)"
	const payload = "dead_code_v1\t0\t0\tfalse\n"
	for name, notes := range map[string][]string{"known": {note}, "unknown": {"Session note: arbitrary"}, "duplicate": {note, note}, "changed": {note + " trailing"}, "second_payload": {payload}} {
		t.Run(name, func(t *testing.T) {
			content := []any{map[string]any{"type": "text", "text": payload}}
			for _, text := range notes {
				content = append(content, map[string]any{"type": "text", "text": text})
			}
			raw, err := json.Marshal(map[string]any{"content": content})
			if err != nil {
				t.Fatal(err)
			}
			before := string(raw)
			_, err = decodeEditProbeOutput(raw, "compact", "analyze.dead_code")
			if (err == nil) != (name == "known") {
				t.Fatalf("session-note boundary %s: %v", name, err)
			}
			if string(raw) != before {
				t.Fatal("raw response changed")
			}
		})
	}
	if _, err := decodeEditProbeOutput(outputWire(payload+note+"\n", nil), "compact", "analyze.dead_code"); err == nil {
		t.Fatal("note text inside producer records was silently discarded")
	}
}

func TestEditConsumerCompactKeepsBoundedPendingPaths(t *testing.T) {
	text := "relations: pending (17 files)\nverify_v1\ttrue\t0\t0\t0\t0\n"
	for i := 0; i < 16; i++ {
		text += fmt.Sprintf("pending_file %q\n", fmt.Sprintf("file%d.go", i))
	}
	body, err := decodeEditProbeOutput(outputWire(text, nil), "compact", "change.verify")
	if err != nil {
		t.Fatal(err)
	}
	if !editConsumerPending(body, "file0.go", "relations") || editConsumerPending(body, "file16.go", "relations") || len(body["pending_files"].([]any)) != 16 {
		t.Fatal("capped scope invented an omitted path or lost an actual path")
	}
	if _, err := decodeEditProbeOutput(outputWire(text+"pending_file \"file16.go\"\n", nil), "compact", "change.verify"); err == nil {
		t.Fatal("producer path cap silently expanded")
	}
}

func TestEditConsumerCompactDoesNotInventIdleFreshness(t *testing.T) {
	for _, variant := range []string{"subject", "absent", "pending"} {
		t.Run(variant, func(t *testing.T) {
			c, plan := editConsumerTestFixture(t)
			plan.Mode = "compact"
			plan.Steps[0].ProbeMode = "compact"
			step := plan.Steps[0]
			text := "verify_v1\ttrue\t0\t0\t0\t0\n"
			if variant == "subject" {
				text = fmt.Sprintf("verify_v1\tfalse\t1\t0\t1\t0\nviolation\t%q\t%q\t%q\t%q\t3\t%q\n", "caller_mismatch", c.File+"::"+step.Marker, step.Marker, c.File, "missing parameter")
			}
			if variant == "pending" {
				text = "relations: pending (1 files)\n" + text + fmt.Sprintf("pending_file %q\n", c.File)
			}
			sink := newEditProbeHeldSink(t)
			sink.unblock()
			s := startHeldEditProbeSpool(t, sink)
			s.step, s.record = step, newEditProbeRecord(c, step)
			origin := time.Now()
			s.started, s.acknowledged = origin, origin.Add(time.Millisecond)
			r := &editProbeRuntime{origin: origin, plan: plan, p: &privateRun{Repos: []privateRepo{{Corpus: c}}}, phase: &Phase{EditProbe: newEditProbeReport(plan, c, true)}}
			identity := wireCallIdentity{WriteStarted: origin.Add(2 * time.Millisecond), Finished: origin.Add(3 * time.Millisecond)}
			oracle := editConsumerOracles(c)[1]
			oracle.call = editProbeWithOutput(oracle.call, "compact")
			if err := r.observeConsumer(s, oracle, outputWire(text, nil), identity, nil); err != nil {
				t.Fatal(err)
			}
			row := s.record.Consumers["consumer_verify"]
			want := map[string]string{"subject": "subject_observed", "absent": "unassessed", "pending": "pending"}[variant]
			if row.State != want || row.SilentStale.Value != nil {
				t.Fatalf("compact freshness invented: %+v want=%s", row, want)
			}
			if len(s.record.FirstCorrect) != 7 {
				t.Fatal("consumer changed visibility denominators")
			}
		})
	}
}
