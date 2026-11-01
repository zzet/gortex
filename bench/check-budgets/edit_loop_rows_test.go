package main

import (
	"os"
	"slices"
	"testing"
)

func TestBudgetsDeclareEveryEditLoopRow(t *testing.T) {
	data, err := os.ReadFile("../budgets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPolicy(data)
	if err != nil {
		t.Fatal(err)
	}
	requireRow := func(scenario, metric string, target float64, unit, compare string) {
		t.Helper()
		for _, row := range p.Budgets {
			if row.Scenario != scenario || row.Metric != metric || row.HostClass != "reference_linux" || !slices.Contains(row.Lane, "edit-smoke") {
				continue
			}
			if row.Mode != "block" || row.OwningRelease != "v0.64.12" || row.Target == nil || *row.Target != target || row.Unit != unit || row.Compare != compare {
				t.Errorf("incorrect edit-loop row: %+v", row)
			}
			return
		}
		t.Errorf("missing absolute edit-smoke row %s/%s", scenario, metric)
	}
	for _, cell := range []struct {
		scenario  string
		ack, read float64
	}{
		{"loadedit-1", .15, .25}, {"loadedit-4", .20, .35}, {"loadedit-8", .25, .40},
		{"loadedit-16", .35, .55}, {"loadedit-32", .50, .75}, {"multirepoedit-6", .30, 0},
	} {
		requireRow(cell.scenario, "edit_ack_p95", cell.ack, "s", "le")
		requireRow(cell.scenario, "edit_ack_max", 1, "s", "le")
		if cell.read > 0 {
			requireRow(cell.scenario, "readback_p95", cell.read, "s", "le")
		}
		requireRow(cell.scenario, "readback_max", 1, "s", "le")
		requireRow(cell.scenario, "readback_correct", .999, "fraction", "ge")
		requireRow(cell.scenario, "reclaim_hold_p95", .05, "s", "le")
		requireRow(cell.scenario, "read_gate_closures", 0, "count", "eq")
		requireRow(cell.scenario, "wal_peak_mib", 2048, "MiB", "le")
		for _, mode := range []string{"json", "toon", "compact", "facade", "cli"} {
			requireRow(cell.scenario, "silent_stale_"+mode, 0, "count", "eq")
		}
	}

	for _, cell := range []struct {
		scenario string
		walP95   float64
	}{
		{"loadedit-1", 1007}, {"loadedit-4", 1059}, {"loadedit-8", 960}, {"multirepoedit-6", 760},
	} {
		requireRow(cell.scenario, "wal_p95_mib", cell.walP95, "MiB", "le")
	}
	requireRow("loadedit-4-largefile", "edit_ack_p95", 0.6, "s", "le")
	requireRow("loadedit-4-largefile", "edit_ack_max", 1, "s", "le")
}

func TestBudgetTargetsMayDifferOnlyAcrossDisjointLanes(t *testing.T) {
	p, err := loadPolicy([]byte(policyFixture))
	if err != nil {
		t.Fatal(err)
	}
	additional := p.Budgets[0]
	threshold := 0.1
	additional.Target = &threshold
	additional.Lane = []string{"edit-smoke"}
	p.Budgets = append(p.Budgets, additional)
	if err := p.validate(); err != nil {
		t.Fatalf("separate edit-probe lane cannot declare its own absolute target: %v", err)
	}
	p.Budgets[len(p.Budgets)-1].Lane = []string{"nightly"}
	if err := p.validate(); err == nil {
		t.Fatal("overlapping lane targets must remain invalid")
	}
}
