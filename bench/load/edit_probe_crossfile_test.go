package main

import (
	"strings"
	"testing"
)

func editProbeCrossFileCorpus(t *testing.T, language string) Corpus {
	t.Helper()
	c := editProbeSymbolCorpus(t, language)
	c.EditKind = "add_crossfile_call"
	ext := c.File[strings.LastIndex(c.File, "."):]
	c.EditCallee = "hub" + ext + "::Hub"
	c.EditTemplate = strings.ReplaceAll(c.EditTemplate, "Example()", "Hub()")
	c.EditInitialContent = editProbeCrossFileImport(c) + c.EditInitialContent
	return c
}
func TestEditProbeCrossFileCallsBindActualOtherFile(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		for _, tool := range []string{"edit.file", "edit.write", "edit.symbol", "edit.batch"} {
			t.Run(language+"/"+tool, func(t *testing.T) {
				c := editProbeCrossFileCorpus(t, language)
				c.EditTool = tool
				plan, err := makeEditProbePlan(c, 42, 100)
				if err != nil {
					t.Fatal(err)
				}
				if plan.Callee != "Hub" || len(plan.Steps) != 100 {
					t.Fatal("crossfile plan identity wrong")
				}
				step := plan.Steps[0]
				oracles := editProbeOracles(c, step)
				if oracles[5].call.Arguments["target"].(map[string]any)["symbol"] != c.EditCallee {
					t.Fatal("callers queried original same-file anchor")
				}
				hubFile, _, _ := editProbeCrossFileCallee(c)
				edge := map[string]any{"from": "owned/" + c.File + "::" + step.Marker, "to": "owned/" + hubFile + "::Hub", "kind": "calls"}
				if !editProbeCorrect("call_chain", map[string]any{"edges": []any{edge}}, c, step, plan.Callee) {
					t.Fatal("actual crossfile call refused")
				}
				edge["to"] = "owned/" + c.File + "::Hub"
				if editProbeCorrect("call_chain", map[string]any{"edges": []any{edge}}, c, step, plan.Callee) {
					t.Fatal("same-file homonym borrowed as crossfile evidence")
				}
				source := editProbeCrossFilePlain(c, c.EditInitialContent) + strings.ReplaceAll(c.EditTemplate, "{{marker}}", step.Marker)
				if !editProbeCorrect("file", map[string]any{"path": "owned/" + c.File, "content": editProbeCrossFileImport(c) + source}, c, step, plan.Callee) {
					t.Fatal("actual explicit import/call file refused")
				}
			})
		}
	}
}
func TestEditProbeCrossFileRefusesAmbiguousImportAndCallee(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			base := editProbeCrossFileCorpus(t, language)
			for _, variant := range []string{"same_file", "outside", "nested_edited", "wrong_language", "missing", "wrong_import"} {
				t.Run(variant, func(t *testing.T) {
					c := base
					switch variant {
					case "same_file":
						c.EditCallee = c.File + "::Hub"
					case "nested_edited":
						c.File = "nested/" + c.File
						c.Symbol = "nested/" + c.Symbol
					case "outside":
						c.EditCallee = "../hub.go::Hub"
					case "wrong_language":
						c.EditCallee = "hub.rs::Hub"
					case "missing":
						c.EditCallee = ""
					case "wrong_import":
						if language == "go" {
							c.EditCallee = "bad-module.go::Hub"
						} else {
							c.EditInitialContent = strings.Replace(c.EditInitialContent, editProbeCrossFileImport(c), "", 1)
						}
					}
					if _, err := makeEditProbePlan(c, 42, 1); err == nil {
						t.Fatal("unsupported crossfile fixture admitted")
					}
				})
			}
		})
	}
}
