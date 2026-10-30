package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEditProbeDeleteUsesRealEmptySpliceAndSeparateAbsence(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			c := editProbeRenameCorpus(t, language, 100)
			c.EditKind = "delete"
			plan, err := makeEditProbePlan(c, 42, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range plan.Steps {
				if !step.Deleted || step.Request.Arguments["replacement"] != "" || strings.Count(c.EditInitialContent, step.Request.Arguments["match"].(string)) != 1 {
					t.Fatal("delete did not use actual exact empty symbol splice")
				}
				oracles := editProbeOracles(c, step)
				if len(oracles) != 7 || oracles[0].call.ExpectedAbsentSymbol != step.BeforeSymbol {
					t.Fatal("delete source absence not explicit")
				}
				if oracles[6].call.Arguments["target"].(map[string]any)["symbol"] != step.BeforeSymbol {
					t.Fatal("deleted call-chain target changed")
				}
			}
		})
	}
}

func TestEditProbeDeleteRequiresCompleteSurvivorAndRelationEvidence(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			c := editProbeRenameCorpus(t, language, 1)
			c.EditKind = "delete"
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			step := plan.Steps[0]
			old := step.Marker + "Before"
			after := strings.Replace(c.EditInitialContent, step.Request.Arguments["match"].(string), "", 1)
			nodes := []any{map[string]any{"id": "owned/" + c.Symbol}}
			for _, oracle := range []string{"file", "summary", "editing_context", "search_visible", "text", "callers", "call_chain"} {
				t.Run(oracle, func(t *testing.T) {
					body := map[string]any{"content": after, "path": "owned/" + c.File, "nodes": nodes, "edges": []any{}, "total_nodes": json.Number("1"), "total_edges": json.Number("0"), "truncated": false, "defines": nodes, "file": map[string]any{"id": "owned/" + c.File}, "results": []any{}, "total": json.Number("0"), "query_class": "symbol", "matches": []any{}, "count": json.Number("0"), "query": old}
					if oracle == "call_chain" {
						body["nodes"] = []any{}
						body["total_nodes"] = json.Number("0")
					}
					if !editProbeCorrect(oracle, body, c, step, plan.Callee) {
						t.Fatalf("complete deletion proof refused: %v", body)
					}
					for _, bad := range []string{"empty", "truncated", "stale_old", "missing_collection"} {
						t.Run(bad, func(t *testing.T) {
							raw, _ := json.Marshal(body)
							var invalid map[string]any
							decoder := json.NewDecoder(strings.NewReader(string(raw)))
							decoder.UseNumber()
							_ = decoder.Decode(&invalid)
							switch bad {
							case "empty":
								invalid = map[string]any{}
							case "truncated":
								invalid["truncated"] = true
								invalid["_truncated_by_limit"] = true
								if oracle == "file" {
									invalid["content"] = ""
								}
							case "stale_old":
								invalid["content"] = c.EditInitialContent
								invalid["nodes"] = append([]any{map[string]any{"id": "owned/" + step.BeforeSymbol}}, nodes...)
								invalid["total_nodes"] = json.Number("2")
								invalid["defines"] = invalid["nodes"]
								invalid["results"] = []any{map[string]any{"id": "owned/" + step.BeforeSymbol}}
								invalid["total"] = json.Number("1")
								invalid["matches"] = []any{map[string]any{"text": old}}
								invalid["count"] = json.Number("1")
							case "missing_collection":
								for _, key := range []string{"nodes", "defines", "results", "matches", "content"} {
									delete(invalid, key)
								}
							}
							if editProbeCorrect(oracle, invalid, c, step, plan.Callee) {
								t.Fatalf("%s falsely established deletion", bad)
							}
						})
					}
				})
			}
		})
	}
}

func TestEditProbeCompleteCountAllowsOnlyExplicitProducerNullWithExactZero(t *testing.T) {
	for _, test := range []struct {
		name string
		body map[string]any
		want bool
	}{
		{"explicit_null", map[string]any{"nodes": nil, "total_nodes": json.Number("0")}, true},
		{"empty_array", map[string]any{"nodes": []any{}, "total_nodes": json.Number("0")}, true},
		{"missing_collection", map[string]any{"total_nodes": json.Number("0")}, false},
		{"missing_total", map[string]any{"nodes": nil}, false},
		{"null_nonzero", map[string]any{"nodes": nil, "total_nodes": json.Number("1")}, false},
		{"wrong_type", map[string]any{"nodes": "", "total_nodes": json.Number("0")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, got := editProbeCompleteCount(test.body, "nodes", "total_nodes")
			if got != test.want {
				t.Fatalf("complete=%v", got)
			}
		})
	}
}
