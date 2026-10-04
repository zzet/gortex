package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestInstallDaemonContractAnalysisGate(t *testing.T) {
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "0")
	runtime, err := installDaemonContractAnalysis(nil, zap.NewNop())
	if err != nil || runtime != nil {
		t.Fatalf("inactive gate installed runtime=%#v err=%v", runtime, err)
	}
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "")
	runtime, err = installDaemonContractAnalysis(nil, zap.NewNop())
	if err == nil || runtime != nil {
		t.Fatalf("incomplete runtime accepted=%#v err=%v", runtime, err)
	}
}

func TestContractAnalysisYieldParksForDemandAndCancels(t *testing.T) {
	var demand atomic.Bool
	demand.Store(true)
	checked := make(chan struct{}, 1)
	yield := contractAnalysisYield(func() bool {
		select {
		case checked <- struct{}{}:
		default:
		}
		return demand.Load()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- yield(ctx) }()
	<-checked
	select {
	case err := <-done:
		t.Fatalf("yield ignored active demand: %v", err)
	default:
	}
	demand.Store(false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	demand.Store(true)
	canceled, stop := context.WithCancel(context.Background())
	go func() { done <- yield(canceled) }()
	<-checked
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("yield cancellation=%v", err)
	}
}
