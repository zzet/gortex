package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSmokeDriverKeepsAllFiveStepsAndStrictPrerequisites(t *testing.T) {
	data, err := os.ReadFile("../budgets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPolicy(data)
	if err != nil {
		t.Fatal(err)
	}
	c := fixtureLaneConfig()
	delete(c.Builds, "tag")
	c.Duration, c.PRSmokeDrain = "20s", "120s"
	plan, err := orderedSmokePlan(p, c)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready || len(plan.Prerequisites) == 0 || len(plan.Tasks) != 30 {
		t.Fatal("missing observations were waived or the median-three same-job matrix changed")
	}
	for i, task := range plan.Tasks {
		role := "main"
		if (i/5)%2 == 1 {
			role = "candidate"
		}
		if task.Repeat%2 == 0 {
			if role == "main" {
				role = "candidate"
			} else {
				role = "main"
			}
		}
		if task.Scenario != smokeSteps[i%5] || task.Role != role {
			t.Fatal("five-step/three-repeat role rotation or two-build contract changed")
		}
	}
	missing := p
	missing.Budgets = nil
	for _, row := range p.Budgets {
		if row.Scenario != "idle-stop" {
			missing.Budgets = append(missing.Budgets, row)
		}
	}
	if _, err := orderedSmokePlan(missing, c); err == nil {
		t.Fatal("missing lifecycle step became a smaller passing smoke")
	}
	extra := p
	extra.Budgets = append(append([]budget{}, p.Budgets...), budget{Scenario: "load-1", Metric: "panics", HostClass: hostedSmokeClass, Mode: "block", Lane: []string{"smoke"}})
	if _, err := orderedSmokePlan(extra, c); err == nil {
		t.Fatal("extra selected row was silently omitted by the five-step driver")
	}
}

func TestSmokeManifestUsesContemporarySourceAndLanguageValidEditPlacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := writeSmokeManifest(path, "/owned/fixture/repo"); err != nil {
		t.Fatal(err)
	}
	var corpora []map[string]string
	if err := readStrictJSON(path, &corpora); err != nil {
		t.Fatal(err)
	}
	if len(corpora) != 1 || corpora[0]["commit"] != smokeFixtureRevision || corpora[0]["symbol"] != "internal/graph/graph.go::fnv1aEdge" || !strings.HasPrefix(corpora[0]["edit_anchor"], "func keyOf(") || strings.Count(corpora[0]["edit_template"], "{{marker}}") != 1 {
		t.Fatal("smoke substituted a fixture or inserted declarations before Go imports")
	}
	source := "package graph\nimport \"fmt\"\n" + corpora[0]["edit_anchor"] + strings.ReplaceAll(corpora[0]["edit_template"], "{{marker}}", "GortexLoadMarker")
	if _, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, parser.AllErrors); err != nil {
		t.Fatal(err)
	}
}

func TestSmokeDriverRefusesUnownedExecutionBeforeArtifacts(t *testing.T) {
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	var output bytes.Buffer
	code := smokeDriverCommand([]string{"--candidate-binary", "candidate", "--load-binary", "load", "--main-bundle", "main", "--candidate-builds", "tests", "--fixture", "fixture", "--out", filepath.Join(t.TempDir(), "evidence")}, &output, &output)
	if code != 1 || !strings.Contains(output.String(), "explicit PR workflow") {
		t.Fatalf("unowned execution was admitted: %d %s", code, &output)
	}
}
