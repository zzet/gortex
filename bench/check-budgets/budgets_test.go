package main

import (
	"os"
	"testing"
)

func TestDeclaredBudgetTargetsAndNamedAllowances(t *testing.T) {
	data, err := os.ReadFile("../budgets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPolicy(data)
	if err != nil {
		t.Fatal(err)
	}
	find := func(host, scenario, metric string) budget {
		t.Helper()
		for _, b := range p.Budgets {
			if b.HostClass == host && b.Scenario == scenario && b.Metric == metric {
				return b
			}
		}
		t.Fatalf("missing %s/%s/%s", host, scenario, metric)
		return budget{}
	}
	for _, host := range []string{"reference_linux", "reference_mac", "large_mac"} {
		for _, tc := range []struct {
			scenario, metric string
			target           float64
		}{
			{"loadedit-1", "edit_ack_p95", .15}, {"loadedit-4", "edit_ack_p95", .20}, {"loadedit-8", "edit_ack_p95", .25}, {"loadedit-16", "edit_ack_p95", .35}, {"loadedit-32", "edit_ack_p95", .50},
			{"multirepoedit-6", "edit_ack_p95", .30}, {"loadedit-32", "edit_ack_max", 1},
			{"loadedit-1", "readback_p95", .25}, {"loadedit-4", "readback_p95", .35}, {"loadedit-8", "readback_p95", .40}, {"loadedit-16", "readback_p95", .55}, {"loadedit-32", "readback_p95", .75},
			{"loadedit-4", "readback_correct", .999}, {"loadedit-4", "unflagged_stale", 0},
			{"loadedit-1", "relations_lag_p95", 1.5}, {"loadedit-4", "relations_lag_p95", 3}, {"loadedit-8", "relations_lag_p95", 5}, {"loadedit-8", "derived_lag_p95", 5},
			{"loadedit-4", "reclaim_hold_p95", .05}, {"loadedit-4", "read_gate_closures", 0},
			{"load-16", "busy_fraction", .01}, {"load-32", "busy_fraction", .01}, {"load-32", "read_p95_vs_load8", 2}, {"cancel", "cancel_release_max", .1},
			{"loadedit-4", "search_symbols_p95", .4}, {"loadedit-4", "read_source_p99", .2},
			{"tools-six-gortex", "dead_code_vs_single", 1.2}, {"tools-six-corpus-5", "outline_p50", .3},
			{"loadedit-4", "impact_stale_refusals", 0}, {"loadedit-4", "detect_stale_refusals", 0}, {"worktrees-4", "daemon_cpu_cores", 4.5},
			{"warm-restart", "repo_not_tracked", 0}, {"schema21-upgrade", "repo_not_tracked", 0}, {"cold-start", "repo_not_tracked", 0}, {"warm-restart", "first_ok_after_ready", 1.5},
			{"idle-stop", "stop_seconds", 3}, {"cold-gortex", "stop_seconds", 5}, {"churn-60", "stop_seconds", 5}, {"idle-stop-20", "force_kills", 0},
		} {
			b := find(host, tc.scenario, tc.metric)
			if b.Mode != "block" || b.OwningRelease != "v0.64.12" || b.Target == nil || *b.Target != tc.target {
				t.Errorf("incorrect required v0.64.12 target %+v", b)
			}
		}
		if b := find(host, "idle-stop-20", "force_kills"); b.MinRepeats != 20 {
			t.Fatal("three repetitions cannot prove 20 clean stops")
		}
		if b := find(host, "loadedit-4", "wal_peak_mib"); b.Mode != "guard" || b.OwningRelease != "v0.64.17" || b.Target == nil || *b.Target != 2048 {
			t.Fatal("WAL remains a future guard, not an early hard cap")
		}
		if b := find(host, "loadedit-4", "edit_written_mib"); b.Mode != "guard" || b.OwningRelease != "v0.64.15" || b.Target == nil || *b.Target != 15 {
			t.Fatal("known v0.64.15 per-edit owner/target lost")
		}
		if b := find(host, "cold-gortex", "cold_written_mib"); b.Mode != "guard" || b.OwningRelease != "v0.64.15" {
			t.Fatal("concrete #1073 milestone must override stale #997 cross-reference")
		}
	}
	if len(p.Allowances) != 3 {
		t.Fatal("each named allowance must have its own entry")
	}
}

func TestHostedSmokeOwnsUnchangedTargetsWithoutReferenceCapacity(t *testing.T) {
	data, err := os.ReadFile("../budgets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPolicy(data)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range p.Budgets {
		if row.HostClass != hostedSmokeClass {
			for _, lane := range row.Lane {
				if lane == "smoke" {
					t.Fatal("PR smoke still mislabeled as reference/Mac capacity")
				}
			}
			continue
		}
		count++
		if row.Mode != "block" || len(row.Lane) != 1 || row.Lane[0] != "smoke" || len(row.Allowance) != 0 {
			t.Fatal("hosted smoke relaxed or escaped PR block scope")
		}
		if row.Metric == "read_p95_vs_main" {
			if row.Target == nil || *row.Target != 1.2 {
				t.Fatal("20% relative rule changed")
			}
			continue
		}
		found := false
		for _, reference := range p.Budgets {
			if reference.HostClass == "reference_linux" && reference.Scenario == row.Scenario && reference.Metric == row.Metric {
				found = true
				if reference.Target == nil || row.Target == nil || *reference.Target != *row.Target || reference.Unit != row.Unit || reference.Compare != row.Compare || reference.MinRepeats != row.MinRepeats || reference.Mode != row.Mode || reference.OwningRelease != row.OwningRelease {
					t.Fatalf("smoke target changed: %+v", row)
				}
				break
			}
		}
		if !found {
			t.Fatalf("smoke target lost its unchanged reference counterpart: %+v", row)
		}
	}
	if count != 38 {
		t.Fatalf("smoke required targets missing: %d", count)
	}
	b := budget{Metric: "read_p95", Scenario: "load-16", HostClass: hostedSmokeClass, OwningRelease: "v0.64.12", Mode: "block", Target: pointer(1), Unit: "s", Lane: []string{"nightly"}, Compare: "le"}
	if err := validateBudget(b, nil); err == nil {
		t.Fatal("hosted profile allowed in nightly policy")
	}
}
