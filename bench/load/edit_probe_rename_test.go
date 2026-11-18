package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func editProbeRenameCorpus(t *testing.T, language string, count int) Corpus {
	t.Helper()
	c := editProbeSymbolCorpus(t, language)
	c.EditKind = "rename"
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("GortexEditProbe%016xN%03dBefore", uint64(42), i)
		c.EditInitialContent += strings.ReplaceAll(c.EditTemplate, "{{marker}}", name)
	}
	return c
}

func TestEditProbeRenameUsesPreexistingDeclarationAndExplicitAbsence(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			c := editProbeRenameCorpus(t, language, 100)
			plan, err := makeEditProbePlan(c, 42, 100)
			if err != nil {
				t.Fatal(err)
			}
			additive := c
			additive.EditKind = ""
			original, err := makeEditProbePlan(additive, 42, 100)
			if err != nil {
				t.Fatal(err)
			}
			for i, step := range plan.Steps {
				if step.DueNS != original.Steps[i].DueNS || step.ActivateNS != original.Steps[i].ActivateNS {
					t.Fatal("rename changed offered arrival schedule")
				}
				before := c.File + "::" + step.Marker + "Before"
				if step.BeforeSymbol != before || step.Request.Arguments["target"].(map[string]any)["symbol"] != before {
					t.Fatal("rename did not bind exact original declaration")
				}
				old, _ := step.Request.Arguments["match"].(string)
				replacement, _ := step.Request.Arguments["replacement"].(string)
				if strings.Count(c.EditInitialContent, old) != 1 || strings.Contains(replacement, step.Marker+"Before") || !editProbeFixtureSource(c.File, replacement, step.Marker, plan.Callee) {
					t.Fatalf("invalid exact rename splice: %q -> %q", old, replacement)
				}
				oracles := editProbeOracles(c, step)
				if len(oracles) != 8 || oracles[7].name != "old_source_absent" || oracles[7].call.ExpectedAbsentSymbol != before {
					t.Fatal("positive graph/source probes replaced instead of explicit absence added")
				}
			}
		})
	}
}

func TestEditProbeRenameRejectsMissingAndAlreadyRenamedFixtures(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			base := editProbeRenameCorpus(t, language, 1)
			for _, name := range []string{"missing_original", "duplicate_original", "already_new", "wrong_tool", "unsupported_kind"} {
				t.Run(name, func(t *testing.T) {
					c := base
					old := strings.TrimSpace(strings.ReplaceAll(c.EditTemplate, "{{marker}}", "GortexEditProbe000000000000002aN000Before"))
					switch name {
					case "missing_original":
						c.EditInitialContent = strings.Replace(c.EditInitialContent, old, "", 1)
					case "duplicate_original":
						c.EditInitialContent += "\n" + old
					case "already_new":
						c.EditInitialContent += strings.ReplaceAll(c.EditTemplate, "{{marker}}", "GortexEditProbe000000000000002aN000")
					case "wrong_tool":
						c.EditTool = "unsupported.tool"
					case "unsupported_kind":
						c.EditKind = "unsupported_future_kind"
					}
					if _, err := makeEditProbePlan(c, 42, 1); err == nil {
						t.Fatal("unsupported/contradictory rename admitted")
					}
				})
			}
		})
	}
}

func TestEditProbeRenameDeleteUseActualFileWriteSymbolBatchSemantics(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		for _, kind := range []string{"rename", "delete"} {
			for _, tool := range []string{"edit.file", "edit.write", "edit.symbol", "edit.batch"} {
				t.Run(language+"/"+kind+"/"+tool, func(t *testing.T) {
					c := editProbeRenameCorpus(t, language, 100)
					c.EditKind = kind
					c.EditTool = tool
					plan, err := makeEditProbePlan(c, 42, 100)
					if err != nil {
						t.Fatal(err)
					}
					offered := c.EditInitialContent
					for _, step := range plan.Steps {
						before := strings.TrimSpace(strings.ReplaceAll(c.EditTemplate, "{{marker}}", step.Marker+"Before"))
						replacement := strings.TrimSpace(strings.ReplaceAll(c.EditTemplate, "{{marker}}", step.Marker))
						if kind == "delete" {
							replacement = ""
						}
						next := strings.Replace(offered, before, replacement, 1)
						switch tool {
						case "edit.file", "edit.symbol":
							if step.Request.Arguments["match"] != before || step.Request.Arguments["replacement"] != replacement {
								t.Fatal("splice semantics changed")
							}
						case "edit.write":
							if step.Request.Arguments["content"] != next {
								t.Fatal("whole-file offered prefix was regenerated/lost")
							}
						case "edit.batch":
							changes := step.Request.Arguments["changes"].([]any)
							change := changes[0].(map[string]any)
							if len(changes) != 1 || change["op"] != "edit_file" || change["path"] != c.File || change["old_string"] != offered || change["new_string"] != next {
								t.Fatal("batch did not retain exact whole-file offered before/after")
							}
						}
						offered = next
					}
				})
			}
		}
	}
}

func TestEditProbeRenameRejectsRetainedOldCallerBesideNewEdge(t *testing.T) {
	c := editProbeRenameCorpus(t, "go", 1)
	plan, err := makeEditProbePlan(c, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Steps[0]
	from := "owned/" + c.File + "::" + step.Marker
	to := "owned/" + c.Symbol
	old := "owned/" + step.BeforeSymbol
	for _, variant := range []string{"valid", "old_node", "old_edge", "truncated", "missing_total"} {
		t.Run(variant, func(t *testing.T) {
			nodes := []any{map[string]any{"id": from}, map[string]any{"id": to}}
			edges := []any{map[string]any{"from": from, "to": to, "kind": "calls"}}
			body := map[string]any{"nodes": nodes, "edges": edges, "total_nodes": json.Number("2"), "total_edges": json.Number("1"), "truncated": false}
			switch variant {
			case "old_node":
				body["nodes"] = append(nodes, map[string]any{"id": old})
				body["total_nodes"] = json.Number("3")
			case "old_edge":
				body["edges"] = append(edges, map[string]any{"from": old, "to": to, "kind": "calls"})
				body["total_edges"] = json.Number("2")
			case "truncated":
				body["truncated"] = true
			case "missing_total":
				delete(body, "total_nodes")
			}
			if got := editProbeCorrect("callers", body, c, step, plan.Callee); got != (variant == "valid") {
				t.Fatalf("correct=%v", got)
			}
		})
	}
}
