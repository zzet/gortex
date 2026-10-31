package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEditProbeAppendLeafHasNoInventedCallEvidence(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			c := editProbeSymbolCorpus(t, language)
			c.EditKind = "append_leaf"
			c.EditTemplate = strings.ReplaceAll(c.EditTemplate, "Example()", "7")
			plan, err := makeEditProbePlan(c, 42, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range plan.Steps {
				source := strings.ReplaceAll(c.EditTemplate, "{{marker}}", step.Marker)
				if !editProbeLeafSource(c.File, source, step.Marker) {
					t.Fatal("leaf declaration refused")
				}
				oracles := editProbeOracles(c, step)
				if oracles[5].call.Arguments["target"].(map[string]any)["symbol"] != c.File+"::"+step.Marker {
					t.Fatal("leaf callers still queried unrelated callee")
				}
				body := map[string]any{"nodes": []any{map[string]any{"id": "owned/" + c.File + "::" + step.Marker}}, "edges": []any{}, "total_nodes": json.Number("1"), "total_edges": json.Number("0"), "truncated": false}
				if !editProbeCorrect("callers", body, c, step, plan.Callee) || !editProbeCorrect("call_chain", body, c, step, plan.Callee) {
					t.Fatal("complete isolated leaf relation proof refused")
				}
				body["edges"] = []any{map[string]any{"from": "owned/" + c.File + "::" + step.Marker, "to": "owned/" + c.Symbol, "kind": "calls"}}
				body["total_edges"] = json.Number("1")
				if editProbeCorrect("call_chain", body, c, step, plan.Callee) {
					t.Fatal("outgoing call accepted as leaf")
				}
			}
		})
	}
}

func TestEditProbeLeafRejectsCommentStringCallAndPartialRelationEvidence(t *testing.T) {
	for _, test := range []struct{ file, source string }{
		{"file.go", "// func Leaf() int { return 1 }"},
		{"file.go", "func Leaf() int { return Example() }"},
		{"file.go", "func Leaf() string { return \"Example()\" }"},
		{"file.ts", "// export function Leaf(): number { return 1; }"},
		{"file.ts", "export function Leaf(): number { return Example(); }"},
		{"file.py", "# def Leaf():\n#     return 1"},
		{"file.py", "def Leaf():\n    return Example()"},
	} {
		if editProbeLeafSource(test.file, test.source, "Leaf") {
			t.Fatalf("non-leaf proof accepted: %s", test.source)
		}
	}
	c := editProbeCorpus()
	step := editProbeStep{Marker: "Leaf"}
	for _, body := range []map[string]any{
		{},
		{"nodes": []any{}, "edges": []any{}, "total_nodes": json.Number("0"), "total_edges": json.Number("0"), "truncated": false},
		{"nodes": []any{map[string]any{"id": c.File + "::Leaf"}}, "edges": []any{}, "total_nodes": json.Number("1"), "total_edges": json.Number("0"), "truncated": true},
	} {
		if editProbeLeafRelations(body, c, step) {
			t.Fatalf("partial/empty relation page accepted:%v", body)
		}
	}
}
