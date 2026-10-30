package main

import (
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
						c.EditTool = "edit.write"
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
