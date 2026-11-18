package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func editProbeSymbolCorpus(t *testing.T, language string) Corpus {
	t.Helper()
	c := editProbeWriteCorpus(t, language)
	c.EditTool = "edit.symbol"
	switch language {
	case "go":
		c.EditAnchor = "func Example() int { return 1 }"
	case "typescript":
		c.EditAnchor = "export function Example(): number { return 1; }"
	case "python":
		c.EditAnchor = "def Example():\n    return 1"
	}
	return c
}
func TestEditProbeSymbolUsesActualDeclarationAndSameOfferedSchedule(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			c := editProbeSymbolCorpus(t, language)
			plan, err := makeEditProbePlan(c, 42, 100)
			if err != nil {
				t.Fatal(err)
			}
			ordinary := c
			ordinary.EditTool = "edit.file"
			ordinary.EditInitialContent = ""
			reference, err := makeEditProbePlan(ordinary, 42, 100)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Tool != "edit.symbol" || len(plan.Steps) != 100 {
				t.Fatal("tool/denominator mismatch")
			}
			for i, step := range plan.Steps {
				if step.DueNS != reference.Steps[i].DueNS || step.ActivateNS != reference.Steps[i].ActivateNS {
					t.Fatal("changed offered schedule")
				}
				req := step.Request
				if req.name() != "edit.symbol" || req.Arguments["target"].(map[string]any)["symbol"] != c.Symbol || req.Arguments["match"] != c.EditAnchor {
					t.Fatal("wrong public scoped splice", req)
				}
				offered := req.Arguments["replacement"].(string)
				if !strings.HasPrefix(offered, c.EditAnchor) || !editProbeFixtureSource(c.File, offered, step.Marker, plan.Callee) {
					t.Fatal("replacement does not preserve selected declaration and add real call")
				}
				args := dispatchArgs(req, privateRepo{Prefix: "owned"})
				if args["target"].(map[string]any)["symbol"] != "owned/"+c.Symbol || req.Arguments["target"].(map[string]any)["symbol"] != c.Symbol {
					t.Fatal("wrong source binding or mutated plan")
				}
			}
		})
	}
}
func TestEditProbeSymbolRefusesForeignCommentOnlyAndPartialAnchors(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		c := editProbeSymbolCorpus(t, language)
		for _, name := range []string{"wrong_symbol", "wrong_file", "comment_anchor", "partial_body", "missing_initial"} {
			t.Run(language+"/"+name, func(t *testing.T) {
				invalid := c
				switch name {
				case "wrong_symbol":
					invalid.Symbol = strings.Replace(c.Symbol, "Example", "Foreign", 1)
				case "wrong_file":
					invalid.Symbol = "wrong/" + c.Symbol
				case "comment_anchor":
					invalid.EditAnchor = "// pretend declaration"
					invalid.EditInitialContent += "\n" + invalid.EditAnchor
				case "partial_body":
					invalid.EditAnchor = "return 1"
				case "missing_initial":
					invalid.EditInitialContent = ""
				}
				if _, err := makeEditProbePlan(invalid, 42, 1); err == nil {
					t.Fatal("accepted unbound symbol fixture", name)
				}
			})
		}
	}
}

func TestEditSymbolAccountingRequiresOfferedToolAndExplicitParseApplicability(t *testing.T) {
	status := editProbeAccountingFixture()
	status["tool"] = "edit_symbol"
	ack := status["acknowledgement"].(map[string]any)
	ack["parse_scope"] = "not_applicable_text_splice"
	ack["phase_states"].(map[string]any)["parse"] = "not_applicable"
	ack["observed_execution_ms"].(map[string]any)["parse"] = nil
	ack["caller_response_ready_ms"] = json.Number("7")
	if got := checkEditProbeAccountingForTool(status, "receipt-a", "edit.symbol"); got.State != "observed_single_window_checks_pass" {
		t.Fatal("matching actual symbol splice refused", got)
	}
	if got := checkEditProbeAccountingForTool(status, "receipt-a", "edit.file"); got.State == "observed_single_window_checks_pass" {
		t.Fatal("borrowed symbol applicability for a file request")
	}
	status["tool"] = "edit_file"
	if got := checkEditProbeAccountingForTool(status, "receipt-a", "edit.symbol"); got.State == "observed_single_window_checks_pass" {
		t.Fatal("borrowed foreign producer tool")
	}
}
