package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// This opt-in source control materializes the existing planner, without starting
// a daemon, cloning an input, or changing the offered schedule.
func TestSearchOuterUsesActualFinitePlanner(t *testing.T) {
	manifest := os.Getenv("GORTEX_SEARCH_OUTER_MANIFEST")
	output := os.Getenv("GORTEX_SEARCH_OUTER_PLAN_OUTPUT")
	if manifest == "" && output == "" {
		t.Skip("explicit owned manifest/output not requested")
	}
	if manifest == "" || output == "" {
		t.Fatal("both manifest and fresh output are required")
	}
	corpora, err := loadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	type cell struct {
		Editors   int           `json:"editors"`
		Counts    []int         `json:"assigned_per_editor"`
		LastDueNS int64         `json:"last_due_ns"`
		BudgetNS  int64         `json:"workload_budget_ns"`
		PlanSHA   string        `json:"plan_sha256"`
		Plan      editProbePlan `json:"plan"`
	}
	var cells []cell
	for _, editors := range []int{1, 4, 8} {
		args := searchAcceptanceArgs(editors, 1000)
		args = append(args, "--search-readback-acceptance", "--edit-mean=1s", "--edit-ack=local", "--out="+output)
		options, err := parseRun(args)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := planEditProbeRoutes(corpora, options)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Steps) != 1000 || plan.Count != 1000 || !plan.SearchReadbackAcceptance {
			t.Fatalf("incomplete actual plan: editors=%d steps=%d", editors, len(plan.Steps))
		}
		counts := make([]int, editors)
		seen := make(map[int]bool)
		for _, step := range plan.Steps {
			if step.Editor < 0 || step.Editor >= editors || seen[step.Ordinal] {
				t.Fatalf("invalid assignment: %+v", step)
			}
			counts[step.Editor]++
			seen[step.Ordinal] = true
		}
		for _, count := range counts {
			if count == 0 {
				t.Fatalf("uncovered editor: %v", counts)
			}
		}
		budget, err := plan.budget(120*time.Second, 1)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		cells = append(cells, cell{editors, counts, plan.Steps[len(plan.Steps)-1].DueNS, int64(budget), hex.EncodeToString(sum[:]), plan})
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(cells); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "ACTUAL_FINITE_PLANS_ONLY_NO_RUNTIME_OR_ACCEPTANCE")
}
