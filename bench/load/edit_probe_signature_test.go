package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func editProbeSignatureCorpus(t *testing.T, language string, count int) Corpus {
	t.Helper()
	c := editProbeSymbolCorpus(t, language)
	c.EditKind = "change_signature"
	switch language {
	case "go":
		c.EditBeforeTemplate = "func {{marker}}() int { return Example() }"
		c.EditTemplate = "func {{marker}}(optional ...int) int {\n return Example()\n}"
	case "typescript":
		c.EditBeforeTemplate = "export function {{marker}}(): number { return Example(); }"
		c.EditTemplate = "export function {{marker}}(optional?: number): number {\n return Example();\n}"
	case "python":
		c.EditBeforeTemplate = "def {{marker}}():\n    return Example()"
		c.EditTemplate = "def {{marker}}(optional=0):\n\n    return Example()"
	}
	for i := 0; i < count; i++ {
		marker := fmt.Sprintf("GortexEditProbe%016xN%03d", uint64(42), i)
		c.EditInitialContent += "\n\n" + strings.ReplaceAll(c.EditBeforeTemplate, "{{marker}}", marker+"Before") + "\n\n" + editProbeFollowingCaller(c.File, marker+"After", marker+"Before")
	}
	return c
}
func TestEditProbeSignaturePreservesActualZeroArgumentCaller(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		for _, tool := range []string{"edit.file", "edit.symbol", "edit.write", "edit.batch"} {
			t.Run(language+"/"+tool, func(t *testing.T) {
				c := editProbeSignatureCorpus(t, language, 100)
				c.EditTool = tool
				plan, err := makeEditProbePlan(c, 42, 100)
				if err != nil {
					t.Fatal(err)
				}
				for _, step := range plan.Steps {
					target := step.Marker + "Before"
					following := editProbeFollowingCaller(c.File, step.Marker+"After", target)
					if strings.Count(c.EditInitialContent, following) != 1 || step.TargetSymbol != c.File+"::"+target {
						t.Fatal("signature fixture lost actual unchanged caller")
					}
					before := strings.ReplaceAll(c.EditBeforeTemplate, "{{marker}}", target)
					after := strings.ReplaceAll(c.EditTemplate, "{{marker}}", target)
					if !editProbeSignatureHeaders(c.File, before, after, target) || !editProbeFixtureSource(c.File, editProbeSignaturePlain(c.File, after), target, plan.Callee) {
						t.Fatal("signature fixture is not the supported optional parameter")
					}
					oracles := editProbeOracles(c, step)
					if oracles[4].call.Arguments["query"] != "optional" {
						t.Fatal("signature text probe could pass on old unchanged body")
					}
				}
			})
		}
	}
}
func TestEditProbeSignatureRequiresChangedSignatureAndOwnSpan(t *testing.T) {
	c := editProbeSignatureCorpus(t, "go", 1)
	plan, err := makeEditProbePlan(c, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps[0]
	target := map[string]any{"id": "owned/" + step.TargetSymbol, "start_line": json.Number("10"), "end_line": json.Number(fmt.Sprint(10 + step.ExpectedLineGap - 2)), "signature": "func target(optional ...int) int", "meta": map[string]any{"signature": "func target(optional ...int) int"}}
	follower := map[string]any{"id": "owned/" + step.FollowingSymbol, "start_line": json.Number(fmt.Sprint(10 + step.ExpectedLineGap))}
	for _, oracle := range []string{"summary", "editing_context", "search_visible"} {
		key := map[string]string{"summary": "nodes", "editing_context": "defines", "search_visible": "results"}[oracle]
		if !editProbeCorrect(oracle, map[string]any{key: []any{target, follower}}, c, step, plan.Callee) {
			t.Fatal("valid changed signature/gap refused")
		}
	}
	delete(target, "signature")
	delete(target, "meta")
	for _, oracle := range []string{"summary", "editing_context"} {
		key := map[string]string{"summary": "nodes", "editing_context": "defines"}[oracle]
		if editProbeCorrect(oracle, map[string]any{key: []any{target, follower}}, c, step, plan.Callee) {
			t.Fatal("line movement borrowed as actual signature metadata")
		}
	}
	edge := map[string]any{"from": "owned/" + step.TargetSymbol, "to": "owned/" + c.Symbol, "kind": "calls"}
	body := map[string]any{"nodes": []any{target}, "edges": []any{edge}}
	if !editProbeCorrect("call_chain", body, c, step, plan.Callee) {
		t.Fatal("actual grown target range refused")
	}
	target["end_line"] = json.Number("10")
	if editProbeCorrect("call_chain", body, c, step, plan.Callee) {
		t.Fatal("preexisting call edge alone proved new signature")
	}
}
