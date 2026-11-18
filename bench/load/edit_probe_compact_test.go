package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEditProbeCompactRowsRequireExactGrammarAndRefuseJSON(t *testing.T) {
	body, err := decodeEditProbeOutput(outputWire("function Added fixture/main.go:5\nfunction Example fixture/main.go:3\n... truncated (9 total)\nedges: 2 of 4 total, 2 EXTRACTED\nconcurrency fixture/main.go::Added: sync_guarded\n", nil), "compact", "relations.callers")
	if err != nil {
		t.Fatal(err)
	}
	answer := body[editProbeCompactKey].(editProbeCompactAnswer)
	if len(body) != 1 || len(answer.rows) != 2 || !answer.truncated || answer.edges != 2 || answer.rows[0] != (editProbeCompactRow{Kind: "function", Qualified: "Added", Path: "fixture/main.go", Line: 5}) {
		t.Fatalf("compact subgraph rows: %+v", answer)
	}
	if editProbeClaimsFresh(body) {
		t.Fatal("compact rows cannot claim freshness")
	}
	search, err := decodeEditProbeOutput(outputWire("method Marker fixture/main.go:9 (in Type)\n", nil), "compact", "search.symbols")
	if err != nil || search[editProbeCompactKey].(editProbeCompactAnswer).rows[0].Owner != "Type" {
		t.Fatalf("symbol search owner suffix: %v %+v", err, search)
	}
	empty, err := decodeEditProbeOutput(outputWire("", nil), "compact", "search.symbols")
	if err != nil || len(empty[editProbeCompactKey].(editProbeCompactAnswer).rows) != 0 {
		t.Fatalf("an empty compact answer is zero rows: %v", err)
	}
	if _, err := decodeEditProbeOutput(outputWire("", nil), "json", "search.symbols"); err == nil {
		t.Fatal("empty JSON payload accepted")
	}
	for name, refused := range map[string]struct{ text, operation string }{
		"JSON body":            {`{"nodes":[{"id":"fixture/main.go::Added"}]}`, "read.summary"},
		"missing newline":      {"function Added fixture/main.go:5", "read.summary"},
		"unknown line":         {"function Added fixture/main.go:5\nfreshness: fresh\n", "read.summary"},
		"row after footer":     {"edges: 1 total\nfunction Added fixture/main.go:5\n", "trace.call_chain"},
		"two footers":          {"function Added fixture/main.go:5\nedges: 1 total\nedges: 1 total\n", "trace.call_chain"},
		"subgraph owner":       {"function Added fixture/main.go:5 (in Type)\n", "relations.callers"},
		"search footer":        {"function Added fixture/main.go:5\nedges: 1 total\n", "search.symbols"},
		"zero line":            {"function Added fixture/main.go:0\n", "read.summary"},
		"no compact producer":  {"function Added fixture/main.go:5\n", "read.source"},
		"no compact producer2": {"function Added fixture/main.go:5\n", "search.text"},
	} {
		if _, err := decodeEditProbeOutput(outputWire(refused.text, nil), "compact", refused.operation); err == nil {
			t.Fatal("accepted", name)
		}
	}
	failed := json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"function Added fixture/main.go:5\n"}]}`)
	if _, err := decodeEditProbeOutput(failed, "compact", "read.summary"); err == nil {
		t.Fatal("tool failure counted as compact rows")
	}
	// A JSON response cannot supply the typed harness-local answer.
	forged, err := decodeEditProbeOutput(outputWire(`{"\u0000load.compact.rows":{"rows":[]}}`, nil), "json", "read.summary")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := forged[editProbeCompactKey].(editProbeCompactAnswer); ok {
		t.Fatal("JSON forged compact rows")
	}
}

func TestEditProbeCompactPresenceBindsMarkerPathAndCallee(t *testing.T) {
	answer := func(operation string, edges int, rows ...editProbeCompactRow) map[string]any {
		return map[string]any{editProbeCompactKey: editProbeCompactAnswer{operation: operation, rows: rows, edges: edges}}
	}
	for _, c := range []Corpus{editProbeCorpus(), editProbeCrossFileCorpus(t, "go")} {
		c.EditOutput = "compact"
		plan, err := makeEditProbePlan(c, 42, 1)
		if err != nil {
			t.Fatal(err)
		}
		step := plan.Steps[0]
		calleeFile := c.File
		if c.EditKind == "add_crossfile_call" {
			calleeFile, _, _ = editProbeCrossFileCallee(c)
		}
		marker := editProbeCompactRow{Kind: "function", Qualified: step.Marker, Path: "owned/" + c.File, Line: 9}
		callee := editProbeCompactRow{Kind: "function", Qualified: plan.Callee, Path: "owned/" + calleeFile, Line: 3}
		wrongPath, wrongKind, method := marker, marker, marker
		wrongPath.Path, wrongKind.Kind, method.Qualified = "owned/other.go", "variable", "Type."+step.Marker
		owned := marker
		owned.Owner = "Type"
		foreign := callee
		foreign.Path = "owned/other" + filepath.Ext(calleeFile)
		for _, check := range []struct {
			oracle string
			body   map[string]any
			want   bool
		}{
			{"summary", answer("read.summary", 1, callee, marker), true},
			{"summary", answer("read.summary", 1, wrongPath), false},
			{"summary", answer("read.summary", 1, wrongKind), false},
			{"summary", answer("read.summary", 1, method), false},
			{"summary", answer("search.symbols", -1, marker), false},
			{"search_visible", answer("search.symbols", -1, marker), true},
			{"search_visible", answer("search.symbols", -1, owned), false},
			{"callers", answer("relations.callers", 2, marker), true},
			{"callers", answer("relations.callers", -1, marker), false},
			{"callers", answer("relations.callers", 0, marker), false},
			{"call_chain", answer("trace.call_chain", 1, marker, callee), true},
			{"call_chain", answer("trace.call_chain", 1, marker), false},
			{"call_chain", answer("trace.call_chain", 1, marker, foreign), false},
			{"source", answer("read.summary", 1, marker), false},
			{"text", answer("read.summary", 1, marker), false},
		} {
			if got := editProbeCorrect(check.oracle, check.body, c, step, plan.Callee); got != check.want {
				t.Fatalf("%s %s: got %v want %v (%+v)", c.EditKind, check.oracle, got, check.want, check.body)
			}
		}
	}
}

func TestEditProbeCompactModeKeepsUnadvertisedOperationsJSON(t *testing.T) {
	c := editProbeCorpus()
	baseline, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	c.EditOutput = "compact"
	plan, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != "compact" || len(plan.Steps) != len(baseline.Steps) {
		t.Fatal(plan.Mode)
	}
	jsonOutput := map[string]any{"format": "json", "max_bytes": 0}
	compactOutput := map[string]any{"format": "json", "compact": true, "max_bytes": 0}
	for i, step := range plan.Steps {
		if !reflect.DeepEqual(step, baseline.Steps[i]) {
			t.Fatal("compact changed the offered edit, its schedule or identity")
		}
		for _, oracle := range editProbeOracles(c, step) {
			want, output := "json", jsonOutput
			if oracle.name == "summary" || oracle.name == "search_visible" || oracle.name == "callers" || oracle.name == "call_chain" {
				want, output = "compact", compactOutput
			}
			if got := editProbeRequestOutput(oracle.call); got != want {
				t.Fatalf("%s requested %s", oracle.name, got)
			}
			if got := dispatchArgs(oracle.call, privateRepo{Prefix: "owned"})["output"]; !reflect.DeepEqual(got, output) {
				t.Fatalf("%s dispatched %v", oracle.name, got)
			}
		}
	}
	if got := dispatchArgs(request{Tool: "read", Operation: "file", Arguments: map[string]any{"operation": "file"}}, privateRepo{Prefix: "owned"})["output"]; !reflect.DeepEqual(got, jsonOutput) {
		t.Fatalf("default dispatched output changed: %v", got)
	}
	for _, invalid := range []map[string]any{{"compact": false}, {"format": "toon", "compact": true}, {"compact": "true"}} {
		if got := editProbeRequestOutput(request{Arguments: map[string]any{"output": invalid}}); got != "invalid" {
			t.Fatalf("%v read as %s", invalid, got)
		}
	}
	// Compact answers count under compact, whose freshness is unobservable:
	// its stale claimed-fresh count is unavailable, never a measured zero.
	sink := newEditProbeHeldSink(t)
	sink.unblock()
	state := startHeldEditProbeSpool(t, sink)
	state.step, state.record = plan.Steps[0], newEditProbeRecord(c, plan.Steps[0])
	origin := time.Now()
	state.started, state.acknowledged = origin.Add(time.Millisecond), origin.Add(2*time.Millisecond)
	run := &editProbeRuntime{origin: origin, plan: plan, p: &privateRun{Repos: []privateRepo{{Corpus: c, Prefix: "owned"}}}}
	identity := wireCallIdentity{WriteStarted: origin.Add(3 * time.Millisecond), Finished: origin.Add(4 * time.Millisecond)}
	for _, oracle := range editProbeOracles(c, plan.Steps[0]) {
		if oracle.name != "summary" {
			continue
		}
		row := "function " + plan.Steps[0].Marker + " owned/" + c.File + ":9\n"
		if err := run.observeProbe(state, oracle, identity.WriteStarted, outputWire(row, nil), identity, nil, false); err != nil {
			t.Fatal(err)
		}
		if err := run.observeProbe(state, oracle, identity.WriteStarted, outputWire(`{"freshness":"fresh"}`, nil), identity, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if state.record.FirstCorrect["summary"].Value == nil || state.record.StaleClaimedFresh {
		t.Fatalf("compact presence not credited, or a JSON answer to a compact request claimed freshness: %+v", state.record)
	}
	coverage := editProbeOutputCoverage(plan.Mode, run.modeCounts)["compact"]
	if coverage.State != "selected" || coverage.ProbeAnswers != 2 || coverage.DecodeFailures != 1 || coverage.ClaimsFreshObservable || coverage.StaleClaimedFresh.Value != nil || coverage.StaleClaimedFresh.Reason == "" {
		t.Fatalf("compact coverage: %+v", coverage)
	}
	report := newEditProbeReport(plan, c, true)
	if !strings.Contains(strings.Join(report.Limitations, "\n"), "stale-claimed-fresh is unavailable") {
		t.Fatal("compact limitations missing", report.Limitations)
	}
}

func TestEditProbeCompactRefusesAbsenceAndBodyKinds(t *testing.T) {
	leaf := editProbeSymbolCorpus(t, "go")
	leaf.EditKind = "append_leaf"
	leaf.EditTemplate = strings.ReplaceAll(leaf.EditTemplate, "Example()", "7")
	deleted := editProbeRenameCorpus(t, "go", 100)
	deleted.EditKind = "delete"
	for _, c := range []Corpus{leaf, editProbeRenameCorpus(t, "go", 100), deleted, editProbeBodyCorpus(t, "go", "modify_body", 100), editProbeSignatureCorpus(t, "go", 100), moveProbeCorpus(42)} {
		if _, err := makeEditProbePlan(c, 42, 100); err != nil {
			t.Fatal("JSON fixture invalid", c.EditKind, err)
		}
		c.EditOutput = "compact"
		if _, err := makeEditProbePlan(c, 42, 100); err == nil || !strings.Contains(err.Error(), "compact output cannot check "+c.EditKind) {
			t.Fatalf("compact admitted %s: %v", c.EditKind, err)
		}
	}
}
