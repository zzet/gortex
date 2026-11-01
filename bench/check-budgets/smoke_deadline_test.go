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
	if len(budgets) != 1 || budgets[0] != 13*time.Minute {
		t.Fatalf("smoke startup independently cuts off strict enrichment before its parent measurement deadline: %v; want13m maximum, bounded by parent cancellation", budgets)
	}
}

func TestSmokeReadinessSharesRemainingParentDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 13*time.Minute)
		defer cancel()
		require := func(want time.Duration) {
			t.Helper()
			got, err := smokeWarmBudget(ctx)
			if err != nil || got != want {
				t.Fatalf("warm budget=%s err=%v, want%s", got, err, want)
			}
		}
		require(13 * time.Minute)
		time.Sleep(6 * time.Minute)
		require(7 * time.Minute)
		warm, stop := context.WithTimeout(ctx, 7*time.Minute)
		defer stop()
		time.Sleep(7 * time.Minute)
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
		longer, stopLonger := context.WithTimeout(context.Background(), 30*time.Minute)
		defer stopLonger()
		if got, err := smokeWarmBudget(longer); err != nil || got != 13*time.Minute {
			t.Fatalf("smoke expanded aggregate maximum: %s %v", got, err)
		}
	})
}
