package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestEditConsumerProbeOptInPreservesDefaultAndRequiresEligibleTarget(t *testing.T) {
	args := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--edit-agents", "1", "--worktrees", "0"}
	ordinary, err := parseRun(args)
	if err != nil {
		t.Fatal(err)
	}
	c := editProbeCorpus()
	before, err := planEditProbe(c, ordinary)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range before.Steps {
		if got := len(editProbeOracles(c, step)); got != 7 {
			t.Fatalf("ordinary edit gained consumer traffic: %d probes", got)
		}
	}
	opted, err := parseRun(append(args, "--edit-consumer-probes"))
	if err != nil {
		t.Fatalf("explicit opt-in cannot be selected: %v", err)
	}
	if _, err := planEditProbe(c, opted); err == nil || !strings.Contains(err.Error(), "unexported") {
		t.Fatalf("exported Example cannot prove dead-code membership; want explicit unexported eligibility refusal, got %v", err)
	}
	after, err := planEditProbe(c, ordinary)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("opt-in planning altered the ordinary plan: %v", err)
	}
	for _, step := range after.Steps {
		if got := len(editProbeOracles(c, step)); got != 7 {
			t.Fatalf("ordinary edit gained consumer traffic: %d probes", got)
		}
	}
}

func TestEditConsumerProbeEligiblePlanDeclaresAllConsumers(t *testing.T) {
	args := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", editProbeScenario, "--agents", "0", "--edit-agents", "1", "--worktrees", "0", "--edit-consumer-probes"}
	options, err := parseRun(args)
	if err != nil {
		t.Fatal(err)
	}
	c := editProbeCorpus()
	c.Symbol, c.Query = c.File+"::helper", "helper"
	c.EditAnchor = "func helper() int { return 1 }"
	c.EditTemplate = "\nfunc {{marker}}() int { return helper() }\n"
	c.ProposedSignature = "func helper(probe int) int"
	plan, err := planEditProbe(c, options)
	if err != nil {
		t.Fatalf("eligible unexported target refused: %v", err)
	}
	for _, step := range plan.Steps {
		names := map[string]bool{}
		for _, oracle := range editProbeOracles(c, step) {
			names[oracle.name] = true
		}
		for _, name := range []string{"consumer_rename", "consumer_verify", "consumer_dead_code", "consumer_detect"} {
			if !names[name] {
				t.Errorf("offered ordinal%d silently lacks %s", step.Ordinal, name)
			}
		}
	}
}

// These are payload recognizers, not completion or silent-stale verdicts.
// Runtime integration must retain pending/absent/error separately and require
// an observed dead-code baseline before interpreting a membership transition.
func TestEditConsumerPayloadRequiresTypedSubjectEvidence(t *testing.T) {
	c := editProbeCorpus()
	plan, err := makeEditProbePlan(c, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps[0]
	id := c.File + "::" + step.Marker
	verify := map[string]any{
		"clean": false, "checked_callers": json.Number("1"),
		"violations": []any{map[string]any{"symbol_id": id, "file_path": c.File, "line": json.Number("4"), "kind": "caller_mismatch"}},
	}
	detect := map[string]any{
		"scope": "unstaged", "changed_files": []any{c.File},
		"changed_symbols": []any{map[string]any{"id": id, "name": step.Marker, "file_path": c.File}},
	}
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"consumer_verify", verify},
		{"consumer_detect", detect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !editProbeCorrect(tc.name, tc.body, c, step, plan.Callee) {
				t.Error("actual producer collection does not establish the edited subject")
			}
			for _, bad := range []struct {
				name string
				body map[string]any
			}{
				{"text_only", map[string]any{"summary": "clean " + id}},
				{"guidance", map[string]any{"guidance": tc.body}},
				{"query_echo", map[string]any{"symbol_id": id, "query": step.Marker}},
				{"empty", map[string]any{}},
				{"error", map[string]any{"error": "unavailable", "guidance": tc.body}},
			} {
				t.Run(bad.name, func(t *testing.T) {
					if editProbeCorrect(tc.name, bad.body, c, step, plan.Callee) {
						t.Fatal("non-evidence counted as a consumer fact")
					}
				})
			}
		})
	}
}

func TestEditConsumerPayloadRejectsForeignOrContradictoryFacts(t *testing.T) {
	c := editProbeCorpus()
	step := editProbeStep{Marker: "Added"}
	for name, body := range map[string]map[string]any{
		"verify_foreign":             {"clean": false, "checked_callers": json.Number("1"), "violations": []any{map[string]any{"symbol_id": "other.go::Added", "file_path": "other.go", "line": json.Number("4"), "kind": "caller_mismatch"}}},
		"verify_clean_contradiction": {"clean": true, "checked_callers": json.Number("1"), "violations": []any{map[string]any{"symbol_id": c.File + "::Added", "file_path": c.File, "line": json.Number("4"), "kind": "caller_mismatch"}}},
		"detect_foreign":             {"scope": "unstaged", "changed_files": []any{"other.go"}, "changed_symbols": []any{map[string]any{"id": "other.go::Added", "name": "Added", "file_path": "other.go"}}},
		"detect_truncated":           {"scope": "unstaged", "changed_files": []any{c.File}, "changed_symbols": []any{map[string]any{"id": c.File + "::Added", "name": "Added", "file_path": c.File}}, "truncated": true},
	} {
		t.Run(name, func(t *testing.T) {
			oracle := "consumer_detect"
			if strings.HasPrefix(name, "verify_") {
				oracle = "consumer_verify"
			}
			if editProbeCorrect(oracle, body, c, step, "Example") {
				t.Fatal("foreign or contradictory consumer evidence was accepted")
			}
		})
	}
}

func TestEditConsumerRenameRequiresDryRunNewCallerSite(t *testing.T) {
	c := editProbeCorpus()
	step := editProbeStep{Marker: "Added"}
	for _, variant := range []string{"actual", "writes", "foreign", "guidance", "no_site"} {
		t.Run(variant, func(t *testing.T) {
			site := map[string]any{"file": c.File, "line": json.Number("4"), "old_text": "func Added() int { return Example() }", "new_text": "func Added() int { return ExampleRenamed() }", "confidence": "provisional", "reason": "pending reference"}
			body := map[string]any{"old_name": "Example", "new_name": "ExampleRenamed", "dry_run": true, "edits": []any{site}}
			switch variant {
			case "writes":
				body["dry_run"] = false
			case "foreign":
				site["file"] = "other.go"
			case "guidance":
				body = map[string]any{"guidance": body}
			case "no_site":
				body["edits"] = []any{}
			}
			if got := editProbeCorrect("consumer_rename", body, c, step, "Example"); got != (variant == "actual") {
				t.Fatalf("dry-run new-caller-site recognition=%t for %s", got, variant)
			}
		})
	}
}

func TestEditConsumerDeadCodePendingSubjectNeverBecomesEmptyListProof(t *testing.T) {
	c := editProbeCorpus()
	c.Symbol = c.File + "::helper"
	step := editProbeStep{Marker: "Added"}
	for _, variant := range []string{"actual_pending", "empty", "foreign", "definitely_dead", "unlabelled", "truncated", "exported"} {
		t.Run(variant, func(t *testing.T) {
			corpus := c
			candidate := map[string]any{"id": c.Symbol, "name": "helper", "file_path": c.File, "line": json.Number("2"), "kind": "function"}
			body := map[string]any{"dead_code": []any{}, "total": json.Number("0"), "truncated": false, "pending_unknown": []any{candidate}, "pending_unknown_count": json.Number("1"), "graph_freshness": "pending", "freshness_scope": "derived", "complete": false}
			switch variant {
			case "empty":
				body = map[string]any{"dead_code": []any{}, "total": json.Number("0"), "truncated": false}
			case "foreign":
				candidate["id"] = "other.go::helper"
			case "definitely_dead":
				body["dead_code"] = []any{candidate}
				body["total"] = json.Number("1")
			case "unlabelled":
				delete(body, "graph_freshness")
			case "truncated":
				body["truncated"] = true
			case "exported":
				corpus.Symbol = c.File + "::Helper"
				candidate["id"], candidate["name"] = corpus.Symbol, "Helper"
			}
			if got := editProbeCorrect("consumer_dead_code", body, corpus, step, "helper"); got != (variant == "actual_pending") {
				t.Fatalf("typed pending-subject recognition=%t for %s; this is not completion credit", got, variant)
			}
		})
	}
}
