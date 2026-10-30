package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func editProbeTypeScriptCorpus() Corpus {
	c := sampleCorpus()
	c.File, c.Symbol = "main.ts", "main.ts::Example"
	c.EditAnchor = "export function Caller(): number { return Example(); }"
	c.EditTemplate = "\nexport function {{marker}}(): number { return Example(); }\n"
	return c
}

func TestEditProbeTypeScriptExplicitCellAndFixture(t *testing.T) {
	c := editProbeTypeScriptCorpus()
	plan, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Language != "typescript" || plan.Kind != "add_intrafile_call" || plan.Tool != "edit.file" || plan.Mode != "json" || len(plan.Steps) != 100 {
		t.Fatalf("wrong cell: %+v", plan)
	}
	goPlan, err := makeEditProbePlan(editProbeCorpus(), 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := range plan.Steps {
		if plan.Steps[i].DueNS != goPlan.Steps[i].DueNS || plan.Steps[i].ActivateNS != goPlan.Steps[i].ActivateNS {
			t.Fatal("language changed offered traffic")
		}
	}
	fixture, err := os.ReadFile("testdata/edit-probe/typescript/main.ts")
	if err != nil {
		t.Fatal(err)
	}
	valid, _ := editProbeTypeScriptFixture(string(fixture), "", "")
	if !valid || strings.Count(string(fixture), "export function ") != 10 || strings.Count(string(fixture), c.EditAnchor) != 1 {
		t.Fatal("fixture does not meet its explicit syntax/initial symbol gate")
	}
	step := plan.Steps[0]
	source := strings.ReplaceAll(c.EditTemplate, "{{marker}}", step.Marker)
	for oracle, body := range map[string]map[string]any{
		"source": {"id": "sample/main.ts::" + step.Marker, "name": step.Marker, "file_path": "sample/main.ts", "source": source},
		"file":   {"path": "sample/main.ts", "content": string(fixture) + source},
		"text":   {"matches": []any{map[string]any{"path": "sample/main.ts", "line": json.Number("11"), "text": strings.TrimSpace(source)}}},
	} {
		if !editProbeCorrect(oracle, body, c, step, plan.Callee) {
			t.Fatalf("actual %s fixture rejected", oracle)
		}
	}
	for _, oracle := range []string{"callers", "call_chain"} {
		body := map[string]any{"edges": []any{map[string]any{"from": "sample/main.ts::" + step.Marker, "to": "sample/main.ts::Example", "kind": "calls"}}}
		if !editProbeCorrect(oracle, body, c, step, plan.Callee) {
			t.Fatalf("actual %s edge rejected", oracle)
		}
	}
}

func TestEditProbeTypeScriptRejectsFalseSourceAndIdentity(t *testing.T) {
	c := editProbeTypeScriptCorpus()
	plan, err := makeEditProbePlan(c, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps[0]
	source := strings.ReplaceAll(c.EditTemplate, "{{marker}}", step.Marker)
	for name, changed := range map[string]string{
		"wrong_callee": strings.ReplaceAll(source, "Example()", "Other()"),
		"comment_only": "/* " + source + " */",
		"line_comment": "// " + strings.TrimSpace(source),
		"string_only":  "const quoted = `" + source + "`;",
		"stale_body":   strings.ReplaceAll(source, "Example()", "1"),
		"duplicate":    source + source,
		"comment_call": strings.ReplaceAll(source, "return Example();", "return 1; /* Example() */"),
	} {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{"id": "sample/main.ts::" + step.Marker, "name": step.Marker, "file_path": "sample/main.ts", "source": changed, "fresh": true}
			if editProbeCorrect("source", body, c, step, plan.Callee) {
				t.Fatal("false source became actual-call proof")
			}
			file := map[string]any{"path": "sample/main.ts", "content": changed, "fresh": true}
			if editProbeCorrect("file", file, c, step, plan.Callee) {
				t.Fatal("false file became actual-call proof")
			}
			text := map[string]any{"matches": []any{map[string]any{"path": "sample/main.ts", "line": json.Number("1"), "text": changed}}}
			if editProbeCorrect("text", text, c, step, plan.Callee) {
				t.Fatal("comment/string/stale call became text proof")
			}
		})
	}
	for _, field := range []string{"id", "file_path"} {
		body := map[string]any{"id": "sample/main.ts::" + step.Marker, "name": step.Marker, "file_path": "sample/main.ts", "source": source}
		body[field] = "sample/foreign.ts::" + step.Marker
		if editProbeCorrect("source", body, c, step, plan.Callee) {
			t.Fatalf("wrong %s accepted", field)
		}
	}
	if editProbeCorrect("file", map[string]any{"path": "sample/foreign.ts", "content": source}, c, step, plan.Callee) {
		t.Fatal("wrong file accepted")
	}
}
