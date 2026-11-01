package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

func TestSmokeReadinessBudgetDoesNotPreemptMeasurementDeadline(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "smoke_driver.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var budgets []time.Duration
	ast.Inspect(source, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "WarmTimeout" {
			return true
		}
		value, ok := kv.Value.(*ast.BasicLit)
		if !ok {
			t.Fatal("smoke warm timeout must remain an explicit reviewable duration")
		}
		text, err := strconv.Unquote(value.Value)
		if err != nil {
			t.Fatal(err)
		}
		budget, err := time.ParseDuration(text)
		if err != nil {
			t.Fatal(err)
		}
		budgets = append(budgets, budget)
		return true
	})
	if len(budgets) != 1 || budgets[0] != 120*time.Minute {
		t.Fatalf("smoke startup independently cuts off strict enrichment before its parent measurement deadline: %v; want 120m maximum, bounded by parent cancellation", budgets)
	}
}

func TestSmokeReadinessSharesRemainingParentDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Minute)
		defer cancel()
		require := func(want time.Duration) {
			t.Helper()
			got, err := smokeWarmBudget(ctx)
			if err != nil || got != want {
				t.Fatalf("warm budget=%s err=%v, want%s", got, err, want)
			}
		}
		require(120 * time.Minute)
		time.Sleep(6 * time.Minute)
		require(114 * time.Minute)
		warm, stop := context.WithTimeout(ctx, 114*time.Minute)
		defer stop()
		time.Sleep(114 * time.Minute)
		synctest.Wait()
		if !errors.Is(warm.Err(), context.DeadlineExceeded) {
			t.Fatalf("gate escaped parent deadline: %v", warm.Err())
		}
		if _, err := smokeWarmBudget(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired parent admitted startup: %v", err)
		}
	})
}

func TestSmokeReadinessHonorsCancellationAndEarlierDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		budget, err := smokeWarmBudget(ctx)
		if err != nil || budget != 2*time.Minute {
			t.Fatalf("earlier parent deadline overridden: %s %v", budget, err)
		}
		warm, stop := context.WithTimeout(ctx, budget)
		defer stop()
		cancel()
		if !errors.Is(warm.Err(), context.Canceled) {
			t.Fatalf("warm gate ignored parent cancellation: %v", warm.Err())
		}
		if _, err := smokeWarmBudget(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled parent admitted startup: %v", err)
		}
		if _, err := smokeWarmBudget(context.Background()); err == nil {
			t.Fatal("unbounded parent admitted startup")
		}
		longer, stopLonger := context.WithTimeout(context.Background(), 180*time.Minute)
		defer stopLonger()
		if got, err := smokeWarmBudget(longer); err != nil || got != 120*time.Minute {
			t.Fatalf("smoke expanded aggregate maximum: %s %v", got, err)
		}
	})
}

func TestSmokeMeasurementReceiptSeparatesDeferredTargetFromSafetyBound(t *testing.T) {
	started := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                   string
		elapsed                time.Duration
		runErr                 error
		wantPass, wantExceeded bool
	}{
		{"within_original_target", 10 * time.Minute, nil, true, false},
		{"complete_above_deferred_target", 20 * time.Minute, nil, true, true},
		{"cleanup_allowance", 121 * time.Minute, nil, true, true},
		{"operational_bound_exceeded", 122*time.Minute + time.Second, nil, false, true},
		{"canceled_measurement", time.Minute, context.Canceled, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt, err := smokeMeasurementReceipt(started, started.Add(tc.elapsed), tc.runErr)
			if receipt["pass"] != tc.wantPass || (err == nil) != tc.wantPass {
				t.Fatalf("wrong measurement outcome: %v %v", receipt, err)
			}
			if receipt["formal_acceptance"] != false || receipt["timing_acceptance"] != "deferred" || receipt["original_full_job_p95_target_seconds"] != float64(900) || receipt["measurement_bound_seconds"] != float64(7200) || receipt["cleanup_allowance_seconds"] != float64(120) || receipt["entry_exceeded_original_wall_target"] != tc.wantExceeded {
				t.Fatalf("operational bound relabeled as acceptance: %v", receipt)
			}
			if tc.runErr != nil && !errors.Is(err, tc.runErr) {
				t.Fatal("original cancellation/error was lost")
			}
		})
	}
}
