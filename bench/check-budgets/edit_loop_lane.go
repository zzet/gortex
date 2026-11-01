package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"
)

// These are separate repeated smoke runs of one build, each with its own
// <=15m wall limit. The nightly's single-editor Poisson schedule is retained;
// it is not compressed to fit the smoke-only deadline.
func makeEditLoopLanePlan(p policy, c laneConfig) (lanePlan, error) {
	plan := lanePlan{Lane: c.Lane, Execution: "edit-loop", HostClass: c.HostClass, Ready: true, PolicySHA256: policyDigest(p), Prerequisites: []string{}}
	if c.HostClass != "reference_linux" || c.Lane != "smoke" && c.Lane != "nightly" {
		return plan, errors.New("edit gates require the reference Linux host and smoke or nightly lane")
	}
	scenarios := []string{"loadedit-4"}
	if c.Lane == "nightly" {
		scenarios = []string{"loadedit-1", "loadedit-4", "loadedit-8", "loadedit-16", "loadedit-32", "multirepoedit-6", "loadedit-4-largefile"}
	}
	if c.Load == "" || c.Manifest == "" || c.Builds["candidate"].Binary == "" {
		plan.Prerequisites = append(plan.Prerequisites, "pinned runtime configuration and reviewed owned-copy corpus manifest required")
	}
	for _, scenario := range scenarios {
		for repeat := 1; repeat <= max(3, c.Repeats); repeat++ {
			task := laneTask{Scenario: scenario, Role: "candidate", Repeat: repeat, Seed: int64(repeat), EditGate: true, EditCount: 100, ProbeModes: []string{"json", "toon", "compact", "facade", "cli"}}
			if scenario == "loadedit-4-largefile" && c.LargeFileManifest == "" {
				task.Unavailable = "large-file gate needs actual >=10000-line target admission; a scenario alias is not evidence"
			}
			plan.Tasks = append(plan.Tasks, task)
		}
	}
	if c.Lane == "nightly" && c.LargeFileManifest == "" {
		plan.Prerequisites = append(plan.Prerequisites, "separate large_file_manifest required; actual >=10000-line admission is validated from each report")
	}
	plan.Ready = len(plan.Prerequisites) == 0
	return plan, nil
}

func finishEditLoopLaneEvidence(c laneConfig, p policy, plan lanePlan, out string, stdout io.Writer, receipt laneReceipt, index inputs, failures []error) error {
	if err := writeNewJSON(filepath.Join(out, "inputs.json"), index); err != nil {
		failures = append(failures, err)
	}
	d, loadErr := loadInputsChecked(filepath.Join(out, "inputs.json"), validateEditLoopReport)
	scenarios := map[string]bool{}
	for _, task := range plan.Tasks {
		scenarios[task.Scenario] = true
	}
	rows := []editLoopRow{}
	pass := true
	for _, b := range p.Budgets {
		if b.HostClass != c.HostClass || !scenarios[b.Scenario] || !slices.Contains(b.Lane, "edit-smoke") {
			continue
		}
		row := editLoopRow{Metric: b.Metric, Scenario: b.Scenario, State: "UNAVAILABLE", Budget: b.Target, Unit: b.Unit}
		if loadErr != nil {
			row.Reason = loadErr.Error()
		} else {
			row = assessEditLoopRow(b, d)
		}
		if editLoopRowBlocks(row) {
			pass = false
		}
		rows = append(rows, row)
	}
	var table bytes.Buffer
	if err := printEditLoopRows(io.MultiWriter(stdout, &table), rows); err != nil {
		failures = append(failures, err)
	}
	if err := writeNewJSON(filepath.Join(out, "budget-results.json"), struct {
		Pass bool          `json:"pass"`
		Rows []editLoopRow `json:"rows"`
	}{pass, rows}); err != nil {
		failures = append(failures, err)
	}
	if err := writeNewJSON(filepath.Join(out, "budget-table.json"), table.String()); err != nil {
		failures = append(failures, err)
	}
	receipt.BudgetPass = pass
	if !pass || len(rows) == 0 {
		failures = append(failures, errors.New("edit-loop budgets have failed or unavailable rows"))
	}
	if !plan.Ready {
		failures = append(failures, errors.New("edit-loop prerequisites unavailable; see plan"))
	}
	receipt.Finished = time.Now().UTC()
	receipt.WallSeconds = receipt.Finished.Sub(receipt.Started).Seconds()
	err := errors.Join(failures...)
	if err != nil {
		receipt.Error = err.Error()
	}
	return errors.Join(err, writeNewJSON(filepath.Join(out, "lane.json"), receipt), laneSummary("Repeated edit-loop runs", receipt))
}

func printEditLoopRows(output io.Writer, rows []editLoopRow) error {
	fmt.Fprintln(output, "scenario\tmetric\tvalue\tbudget\tunit\tbaseline\tdelta\tstate\treason")
	for _, row := range rows {
		if _, err := fmt.Fprintf(output, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.Scenario, row.Metric, editLoopNumber(row.Value), editLoopNumber(row.Budget), row.Unit, editLoopNumber(row.Baseline), editLoopNumber(row.Delta), row.State, row.Reason); err != nil {
			return err
		}
	}
	return nil
}
