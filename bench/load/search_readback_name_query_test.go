package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The registered producer proof establishes the existing name: clause. This
// control binds the actual LOAD request and decoder over the real pipe: broad
// fuzzy neighbors cannot prove absence, and a name query cannot relax it.
func TestSearchReadbackNameClauseKeepsExactPositiveAndCompleteNegative(t *testing.T) {
	step := editProbeStep{ExpectedSearchLine: 9, Marker: "GortexEditProbe000000000000002aN002", BeforeSymbol: "main.go::GortexEditProbe000000000000002aN002Before"}
	repo := privateRepo{Prefix: "fixture", Corpus: Corpus{File: "main.go"}}
	old := strings.TrimPrefix(step.BeforeSymbol, repo.Corpus.File+"::")
	for _, kind := range []string{"search_visible", "old_search_absent"} {
		for _, incomplete := range []bool{false, true} {
			name := kind + "/complete"
			if incomplete {
				name = kind + "/budget_incomplete"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				wantName := step.Marker
				if kind == "old_search_absent" {
					wantName = old
				}
				seen := make(chan map[string]any, 1)
				client := searchSequenceClient(t, func(frame stubFrame) any {
					args, _ := frame.Params["arguments"].(map[string]any)
					seen <- args
					rows := []any{}
					if kind == "search_visible" {
						rows = append(rows, map[string]any{"id": "fixture/main.go::" + step.Marker, "name": step.Marker, "file_path": "fixture/main.go", "start_line": step.ExpectedSearchLine})
					}
					body := map[string]any{"results": rows, "total": len(rows), "truncated": false}
					// The real broad-query control returned incomplete neighbors.
					// They contain no old ID but cannot certify its absence.
					if incomplete || args["query"] != "name:"+wantName {
						body["_truncated_by_budget"] = true
						body["total"] = 1001
					}
					return stubContent(body)
				})
				sink := newEditProbeHeldSink(t)
				sink.unblock()
				state := startHeldEditProbeSpool(t, sink)
				probe := searchReadbackRequest(kind, repo, step)
				caller := &editProbeCaller{runtime: &editProbeRuntime{origin: time.Now()}, state: state, client: client, purpose: "readback:" + kind, request: probe}
				raw, err := caller.call(ctx, "tools/call", map[string]any{"name": probe.Tool, "arguments": dispatchArgs(probe, repo)})
				if err != nil {
					t.Fatal(err)
				}
				args := <-seen
				if args["query"] != "name:"+wantName || args["operation"] != "symbols" {
					t.Errorf("actual symbol request used a broad or changed form: %v", args)
				}
				options, _ := args["options"].(map[string]any)
				if options["query_class"] != "symbol" || options["expand"] != "off" || options["limit"] != float64(10000) || options["max_bytes"] != float64(0) || options["max_per_file"] != float64(0) {
					t.Errorf("name clause changed existing symbol controls: %v", options)
				}
				if kind == "search_visible" && options["path"] != nil || kind == "old_search_absent" && options["path"] != repo.Corpus.File {
					t.Errorf("name clause changed positive/negative path selection: %v", options)
				}
				body, err := decodeEditProbeOutput(raw, "json", "search.symbols")
				if err != nil {
					t.Fatal(err)
				}
				wantCorrect := kind == "search_visible" || !incomplete
				if got := exactSearchReadback(kind, body, repo, step, ""); got != wantCorrect {
					t.Errorf("exact/complete oracle changed: got=%v want=%v body=%v", got, wantCorrect, body)
				}
				state.finishRaw()
				if state.rawBytes == 0 || state.rawBytes != state.rawAccepted || state.sinkErr != nil {
					t.Fatal("actual request/reply witness was not retained")
				}
				var event struct {
					WireID  int64           `json:"wire_id"`
					Payload json.RawMessage `json:"payload"`
				}
				if err := json.Unmarshal(sink.data.Bytes(), &event); err != nil || event.WireID != caller.last.ID || len(event.Payload) == 0 {
					t.Fatalf("request identity did not own its raw reply: %+v %v", event, err)
				}
			})
		}
	}
}
