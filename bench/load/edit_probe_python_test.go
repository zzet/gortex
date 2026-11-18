package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func editProbePythonCorpus() Corpus {
	c := sampleCorpus()
	c.File, c.Symbol = "main.py", "main.py::Example"
	c.EditAnchor = "def Caller():\n    return Example()"
	c.EditTemplate = "\ndef {{marker}}():\n    return Example()\n"
	return c
}

func TestEditProbePythonExplicitCellAndFixture(t *testing.T) {
	c := editProbePythonCorpus()
	plan, err := makeEditProbePlan(c, 42, 100)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Language != "python" || plan.Kind != "add_intrafile_call" || plan.Tool != "edit.file" || plan.Mode != "json" || len(plan.Steps) != 100 {
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
	fixture, err := os.ReadFile("testdata/edit-probe/python/main.py")
	if err != nil {
		t.Fatal(err)
	}
	valid, _ := editProbePythonFixture(string(fixture), "", "")
	if !valid || strings.Count(string(fixture), "def ") != 10 || strings.Count(string(fixture), c.EditAnchor) != 1 {
		t.Fatal("fixture does not meet explicit syntax/initial symbol gate")
	}
	step := plan.Steps[0]
	source := strings.ReplaceAll(c.EditTemplate, "{{marker}}", step.Marker)
	for oracle, body := range map[string]map[string]any{
		"source": {"id": "sample/main.py::" + step.Marker, "name": step.Marker, "file_path": "sample/main.py", "source": source},
		"file":   {"path": "sample/main.py", "content": string(fixture) + source},
		"text":   {"matches": []any{map[string]any{"path": "sample/main.py", "line": json.Number("31"), "text": "def " + step.Marker + "():"}}},
	} {
		if !editProbeCorrect(oracle, body, c, step, plan.Callee) {
			t.Fatalf("actual %s fixture rejected", oracle)
		}
	}
	for _, oracle := range []string{"callers", "call_chain"} {
		body := map[string]any{"edges": []any{map[string]any{"from": "sample/main.py::" + step.Marker, "to": "sample/main.py::Example", "kind": "calls"}}}
		if !editProbeCorrect(oracle, body, c, step, plan.Callee) {
			t.Fatalf("actual %s edge rejected", oracle)
		}
	}
}

func TestEditProbePythonRejectsFalseSourceAndBoundaries(t *testing.T) {
	c := editProbePythonCorpus()
	plan, err := makeEditProbePlan(c, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps[0]
	source := strings.ReplaceAll(c.EditTemplate, "{{marker}}", step.Marker)
	for name, changed := range map[string]string{
		"wrong_callee":      strings.ReplaceAll(source, "Example()", "Other()"),
		"comment_only":      "# " + strings.ReplaceAll(strings.TrimSpace(source), "\n", "\n# "),
		"string_only":       "\"\"\"" + source + "\"\"\"",
		"stale_body":        strings.ReplaceAll(source, "Example()", "1"),
		"comment_call":      strings.ReplaceAll(source, "return Example()", "return 1 # Example()"),
		"tab_body":          strings.ReplaceAll(source, "    return", "\treturn"),
		"wrong_indent":      strings.ReplaceAll(source, "    return", "  return"),
		"nested_definition": "def Outer():\n    " + strings.ReplaceAll(strings.TrimSpace(source), "\n", "\n    ") + "\n",
		"extra_body":        source + "    return Other()\n",
		"duplicate":         source + source,
	} {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{"id": "sample/main.py::" + step.Marker, "name": step.Marker, "file_path": "sample/main.py", "source": changed, "fresh": true}
			if editProbeCorrect("source", body, c, step, plan.Callee) {
				t.Fatal("false source became actual-call proof")
			}
			if editProbeCorrect("file", map[string]any{"path": "sample/main.py", "content": changed, "fresh": true}, c, step, plan.Callee) {
				t.Fatal("false file became actual-call proof")
			}
		})
	}
	for _, field := range []string{"id", "file_path"} {
		body := map[string]any{"id": "sample/main.py::" + step.Marker, "name": step.Marker, "file_path": "sample/main.py", "source": source}
		body[field] = "sample/foreign.py::" + step.Marker
		if editProbeCorrect("source", body, c, step, plan.Callee) {
			t.Fatalf("wrong %s accepted", field)
		}
	}
	if editProbeCorrect("file", map[string]any{"path": "sample/foreign.py", "content": source}, c, step, plan.Callee) {
		t.Fatal("wrong file accepted")
	}
	for _, text := range []string{"# def " + step.Marker + "():", "\"def " + step.Marker + "():\"", "    def " + step.Marker + "():"} {
		body := map[string]any{"matches": []any{map[string]any{"path": "sample/main.py", "line": json.Number("1"), "text": text}}}
		if editProbeCorrect("text", body, c, step, plan.Callee) {
			t.Fatal("comment/string/nested declaration became top-level text proof")
		}
	}
}

func TestEditProbePythonRejectsInvalidTemplates(t *testing.T) {
	base := editProbePythonCorpus()
	for _, template := range []string{
		strings.ReplaceAll(base.EditTemplate, "Example()", "Other()"),
		strings.ReplaceAll(base.EditTemplate, "    return", "\treturn"),
		"# " + strings.ReplaceAll(strings.TrimSpace(base.EditTemplate), "\n", "\n# "),
		strings.ReplaceAll(base.EditTemplate, "Example()", "\"Example()\""),
	} {
		c := base
		c.EditTemplate = template
		if _, err := makeEditProbePlan(c, 42, 1); err == nil {
			t.Fatal("invalid Python fixture template admitted")
		}
	}
}
