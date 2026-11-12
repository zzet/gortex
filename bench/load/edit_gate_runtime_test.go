package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	toon "github.com/toon-format/toon-go"
)

func TestEditGateRuntimeReadsEveryRequiredKindAndRetainsLiveTiers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c := editProbeCorpus()
	plan, err := makeEditProbePlan(c, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	// A one-record dispatch control, never accepted as the100-edit release gate.
	plan.GateModes = true
	plan.Steps[0].ProbeMode = "json"
	step := plan.Steps[0]
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, c.File), []byte("package fixture\nfunc Example() int { return 1 }\n"+c.EditAnchor+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, func(frame stubFrame) (any, *rpcError) {
		if frame.Method != "tools/call" {
			return stubContent(map[string]any{}), nil
		}
		args, _ := frame.Params["arguments"].(map[string]any)
		if frame.Params["name"] == "edit" && args["operation"] == "file" {
			return stubContent(map[string]any{"mutation_receipt": "receipt-a", "readback_exact": false, "status": "applied", "disk_status": "committed", "mutation_id": step.MutationID, "commit_seq": 7, "content_sha256": strings.Repeat("a", 64)}), nil
		}
		if frame.Params["name"] == "change" && args["operation"] == "receipt" {
			return stubContent(map[string]any{"receipt": "receipt-a", "mutation_id": step.MutationID, "graph_status_terminal": true, "graph_status": "fresh", "ack_at": "2026-01-01T00:00:00Z", "file_graph_at": "2026-01-01T00:00:00Z", "relations_at": "2026-01-01T00:00:00.001Z", "derived_at": "2026-01-01T00:00:00.002Z", "tier_state": map[string]any{"file_graph": "fresh", "relations": "fresh", "derived": "fresh"}}), nil
		}
		// Incorrect empty reads must stay errors, even when coverage is complete.
		return stubContent(map[string]any{}), nil
	})
	run.requireAccounting = false
	run.doEdit(ctx, step)
	state := run.states[0]
	state.finishRaw()
	if state.record.ACKReadbackExact == nil || *state.record.ACKReadbackExact || state.record.ACKStatus != "applied" || state.record.ACKDiskStatus != "committed" || state.record.ACKMutationID != step.MutationID || state.record.ACKCommitSeq != 7 || state.record.ACKContentSHA256 != strings.Repeat("a", 64) {
		t.Fatal("ACK evidence was not retained from the original decoded response")
	}
	if len(state.record.Readbacks) == 0 || state.record.Readbacks[0].Kind != "source" {
		t.Error("edit + one read must end at the first symbol-source/summary response, not read_file")
	}
	seen := map[string]bool{}
	for _, back := range state.record.Readbacks {
		seen[back.Kind] = true
	}
	for _, kind := range []string{"file", "source", "summary", "search_visible", "text"} {
		if !seen[kind] {
			t.Errorf("gate never dispatched required same-editor immediate %s readback", kind)
		}
	}
	if state.record.Readback.Value != nil || state.record.Outcome == "complete" {
		t.Error("incorrect readbacks became a successful gate observation")
	}
	data, err := json.Marshal(state.record)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Tiers *struct {
			ACK       string            `json:"ack_at"`
			File      string            `json:"file_graph_at"`
			Relations string            `json:"relations_at"`
			Derived   string            `json:"derived_at"`
			States    map[string]string `json:"tier_state"`
		} `json:"mutation_tiers"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Tiers == nil {
		t.Error("actual mutation_status tier stamps were discarded")
	} else if wire.Tiers.ACK == "" || wire.Tiers.File != wire.Tiers.ACK || wire.Tiers.Relations == "" || wire.Tiers.Derived == "" {
		t.Errorf("equal-clock causal stamps were lost or invented: %+v", wire.Tiers)
	}
	if wire.Tiers != nil {
		for _, tier := range []string{"file_graph", "relations", "derived"} {
			if wire.Tiers.States[tier] != "fresh" {
				t.Errorf("live tier state %s was discarded: %+v", tier, wire.Tiers.States)
			}
		}
	}
}

func TestEditGateFreshnessPrefixSurvivesActualWireDecoding(t *testing.T) {
	for _, mode := range []string{"compact", "toon", "gcx"} {
		t.Run(mode, func(t *testing.T) {
			for _, state := range []string{"pending", "failed"} {
				var text string
				switch mode {
				case "compact":
					text = "relations: " + state + " (2 files)\nfunction Added fixture/main.go:5\n"
				case "toon":
					encoded, err := toon.Marshal(map[string]any{"nodes": []any{map[string]any{"id": "fixture/main.go::Added"}}, "total": 1, "truncated": false})
					if err != nil {
						t.Fatal(err)
					}
					text = "relations: " + state + " (2 files)\n" + string(encoded)
				case "gcx":
					text = "GCX1 tool=get_file_summary fields=id graph_freshness=" + state + " freshness_scope=relations pending_file_count=2\n# relations: " + state + " (2 files)\nfixture/main.go::Added\n"
				}
				body, err := decodeEditProbeOutput(outputWire(text, map[string]any{"graph_freshness": "fresh"}), mode, "read.summary")
				if mode == "gcx" {
					if !errors.Is(err, errEditOutputGCXUnavailable) {
						t.Fatalf("GCX payload got unrequested decoder credit: %v", err)
					}
				} else if err != nil {
					t.Errorf("actual %s freshness rider rejected: %v", mode, err)
					continue
				}
				if body["graph_freshness"] != state || body["freshness_scope"] != "relations" || body["pending_file_count"] != json.Number("2") {
					t.Errorf("discarded actual producer metadata (%s/%s): %+v", mode, state, body)
				}
				if editProbeClaimsFresh(body) {
					t.Error("pending/failed text rider became a fresh claim")
				}
			}
		})
	}
}

func TestEditGateRuntimeInterpretsEachRecordsActualProbeSurface(t *testing.T) {
	for _, mode := range []string{"toon", "compact", "facade", "cli"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := editProbeCorpus()
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			plan.GateModes = true
			plan.Steps[0].ProbeMode = mode
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.File), []byte("package fixture\n"+c.EditAnchor+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, func(stubFrame) (any, *rpcError) { return stubContent(map[string]any{}), nil })
			state := run.states[0]
			defer state.finishRaw()
			now := time.Now()
			state.started, state.acknowledged = now.Add(-time.Millisecond), now.Add(-time.Microsecond)
			oracle := editProbeOracle{name: "summary", call: request{Tool: "read", Operation: "summary", Arguments: map[string]any{"operation": "summary"}}}
			oracle.call = editProbeWithOutput(oracle.call, mode)
			var raw json.RawMessage
			switch mode {
			case "toon":
				text, err := toon.Marshal(map[string]any{"fresh": true, "nodes": []any{}})
				if err != nil {
					t.Fatal(err)
				}
				raw = outputWire(string(text), nil)
			case "compact":
				raw = outputWire("relations: failed (1 files)\nfunction "+plan.Steps[0].Marker+" fixture/"+c.File+":5\n", nil)
			case "facade":
				raw = outputWire(`{"fresh":true,"nodes":[]}`, nil) // Missing surface identity must not borrow JSON credit.
			case "cli":
				raw = json.RawMessage(`{"fresh":true,"nodes":[]}`)
			}
			if err := run.observeProbe(state, oracle, now, raw, wireCallIdentity{WriteStarted: now, Finished: now.Add(time.Microsecond)}, nil, false); err != nil {
				t.Fatal(err)
			}
			got := run.modeCounts[mode]
			if got.answers != 1 {
				t.Errorf("actual %s answer was not counted in its record's mode: %+v", mode, run.modeCounts)
			}
			if run.modeCounts["json"].answers != 0 {
				t.Error("non-JSON record borrowed JSON observations")
			}
			switch mode {
			case "facade":
				if got.decodeFailures != 1 || got.claimsFresh != 0 {
					t.Error("missing facade identity was interpreted as a fresh JSON answer")
				}
			case "toon", "cli":
				if got.decodeFailures != 0 || got.stale != 1 {
					t.Error("actual decoded stale/fresh claim was discarded or attributed to another format")
				}
			case "compact":
				if got.decodeFailures != 0 || got.claimsFresh != 0 {
					t.Error("explicit failed metadata became a decode error or fresh claim")
				}
			}
		})
	}
}

func TestEditGateRuntimeRequestsTheAssignedWireForEachRecord(t *testing.T) {
	for _, mode := range []string{"toon", "compact"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			c := editProbeCorpus()
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			plan.GateModes = true
			plan.Steps[0].ProbeMode = mode
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.File), []byte("package fixture\n"+c.EditAnchor+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			seen := make(chan map[string]any, 1)
			run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, func(frame stubFrame) (any, *rpcError) {
				args, _ := frame.Params["arguments"].(map[string]any)
				if frame.Method == "tools/call" && frame.Params["name"] == "read" && args["operation"] == "summary" {
					output, _ := args["output"].(map[string]any)
					select {
					case seen <- output:
					default:
					}
				}
				return stubContent(map[string]any{}), nil // JSON cannot stand in for either requested wire.
			})
			state := run.states[0]
			defer state.finishRaw()
			run.probe(ctx, cancel, state)
			var output map[string]any
			select {
			case output = <-seen:
			case <-ctx.Done():
				t.Error("assigned-mode request never reached actual socket dispatch")
			}
			cancel()
			state.probes.Wait()
			if mode == "toon" && output["format"] != "toon" {
				t.Errorf("record sent%v instead of its TOON wire", output)
			}
			if mode == "compact" && output["compact"] != true {
				t.Errorf("record sent%v instead of its compact producer request", output)
			}
		})
	}
}

func TestEditGateModeCoverageCountsDistinctAssignedEditsAndActualTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c := editProbeCorpus()
	plan, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	plan.GateModes = true
	plan.Steps[0].ProbeMode, plan.Steps[1].ProbeMode = "json", "compact"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, c.File), []byte("package fixture\n"+c.EditAnchor+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newEditProbeStubRun(t, ctx, c, plan, dir, 2, func(stubFrame) (any, *rpcError) { return stubContent(map[string]any{}), nil })
	for _, state := range run.states {
		defer state.finishRaw()
	}
	now := time.Now()
	for _, state := range run.states {
		state.started, state.acknowledged = now.Add(-2*time.Millisecond), now.Add(-time.Millisecond)
	}
	identity := wireCallIdentity{WriteStarted: now, Finished: now.Add(time.Microsecond)}
	summary := editProbeOracle{name: "summary", call: request{Tool: "read", Operation: "summary"}}
	for range 2 {
		if err := run.observeProbe(run.states[0], summary, now, outputWire(`{"fresh":true,"nodes":[]}`, nil), identity, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	// Compact does not implement source. Its real JSON fallback must not
	// lend another assigned edit or freshness claim to the JSON cell.
	source := editProbeOracle{name: "source", call: request{Tool: "read", Operation: "source"}}
	if err := run.observeProbe(run.states[1], source, now, outputWire(`{"fresh":true,"nodes":[]}`, nil), identity, nil, false); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(editProbeOutputCoverage("gate", run.modeCounts))
	if err != nil {
		t.Fatal(err)
	}
	var coverage map[string]struct {
		Observed *int `json:"observed_edits"`
		Fresh    int  `json:"claims_fresh"`
		Tools    map[string]struct {
			Answers int `json:"probe_answers"`
			Edits   int `json:"observed_edits"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &coverage); err != nil {
		t.Fatal(err)
	}
	jsonMode := coverage["json"]
	if jsonMode.Observed == nil || *jsonMode.Observed != 1 || jsonMode.Fresh != 2 {
		t.Errorf("repeated probes or another assigned mode borrowed edit/freshness credit: %s", data)
	}
	if tool := jsonMode.Tools["read.summary"]; tool.Answers != 2 || tool.Edits != 1 {
		t.Errorf("actual dispatched tool census missing or inflated: %s", data)
	}
	if compact := coverage["compact"]; compact.Observed == nil || *compact.Observed != 0 {
		t.Errorf("JSON fallback credited as compact: %s", data)
	}
	if _, exists := coverage["facade"]; !exists {
		t.Errorf("facade mode omitted entirely: %s", data)
	}
}

func TestEditGateCompactCoverageRequiresPostACKSemanticAndRefusalProof(t *testing.T) {
	for _, variant := range []string{"pre_ack", "wrong_content", "correct_content", "decode_failure", "unsupported_fallback"} {
		t.Run(variant, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := editProbeCorpus()
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			plan.GateModes = true
			plan.Steps[0].ProbeMode = "compact"
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.File), []byte("package fixture\n"+c.EditAnchor+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, func(stubFrame) (any, *rpcError) { return stubContent(map[string]any{}), nil })
			state := run.states[0]
			defer state.finishRaw()
			now := time.Now()
			state.started, state.acknowledged = now.Add(-3*time.Millisecond), now.Add(-time.Millisecond)
			sent := now
			if variant == "pre_ack" {
				sent = now.Add(-2 * time.Millisecond)
			}
			oracle := editProbeOracle{name: "summary", call: editProbeWithOutput(request{Tool: "read", Operation: "summary"}, "compact")}
			row := "function " + state.step.Marker + " fixture/" + c.File + ":5\n"
			raw := outputWire(row, nil)
			if variant == "wrong_content" {
				raw = outputWire("function Wrong fixture/"+c.File+":5\n", nil)
			}
			if variant == "decode_failure" {
				raw = outputWire(`{"fresh":true}`, nil)
			}
			if variant == "unsupported_fallback" {
				oracle = editProbeOracle{name: "source", call: request{Tool: "read", Operation: "source"}}
				raw = outputWire(`{}`, nil)
			}
			if err := run.observeProbe(state, oracle, sent, raw, wireCallIdentity{WriteStarted: sent, Finished: sent.Add(time.Microsecond)}, nil, false); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(editProbeOutputCoverage("gate", run.modeCounts))
			if err != nil {
				t.Fatal(err)
			}
			var modes map[string]struct {
				Tools map[string]struct {
					Correct  *int   `json:"correct_edits"`
					Refused  *int   `json:"refused_edits"`
					Fallback *int   `json:"fallback_answers"`
					State    string `json:"state"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(data, &modes); err != nil {
				t.Fatal(err)
			}
			if variant == "unsupported_fallback" {
				tool := modes["compact"].Tools["read.source"]
				if tool.State != "UNSUPPORTED" || tool.Refused == nil || *tool.Refused != 1 || tool.Fallback == nil || *tool.Fallback != 1 {
					t.Errorf("actual unsupported fallback/refusal was lost: %s", data)
				}
				return
			}
			tool := modes["compact"].Tools["read.summary"]
			want := 0
			if variant == "correct_content" {
				want = 1
			}
			if tool.Correct == nil || *tool.Correct != want {
				t.Errorf("compact semantic witness borrowed pre-ACK, wrong or undecoded content: %s", data)
			}
		})
	}
}

func TestEditGateRetainsActualAdmittedTargetLineCount(t *testing.T) {
	for _, lines := range []int{9999, 10000, 10001} {
		t.Run(fmt.Sprint(lines), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := editProbeCorpus()
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			plan.GateModes = true
			plan.Steps[0].ProbeMode = "json"
			dir := t.TempDir()
			source := "package fixture\r\n" + strings.Repeat("// padding\r\n", lines-2) + c.EditAnchor
			file := filepath.Join(dir, c.File)
			if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, func(stubFrame) (any, *rpcError) { return stubContent(map[string]any{}), nil })
			defer run.states[0].finishRaw()
			// Later bytes cannot promote an initially small target into the
			// separate large-file admission class.
			if err := os.WriteFile(file, []byte(source+"\n"+strings.Repeat("// later\n", 10000)), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, finish, _ := run.beginEdit(ctx, plan.Steps[0])
			finish()
			data, err := json.Marshal(run.states[0].record)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Lines *int `json:"admitted_target_lines"`
			}
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Lines == nil || *wire.Lines != lines {
				t.Errorf("target class was omitted or borrowed from later bytes: %s", data)
			}
		})
	}
}
