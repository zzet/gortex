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
	for _, mode := range []string{"json", "toon", "compact", "facade", "cli"} {
		requireRow("loadedit-4", "silent_stale_"+mode, 0, "count", "eq")
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
