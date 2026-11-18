package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// TestDaemonContractLinkedScheduleFollowsConfigAndEnvironment: the daemon
// takes the linked policy from the contract_analysis block, lets the
// environment override it, and starts on the defaults with a warning when a
// setting does not parse.
func TestDaemonContractLinkedScheduleFollowsConfigAndEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.ContractAnalysisConfig
		env  string
		want indexer.ContractLinkedSchedule
		warn bool
	}{
		{name: "default", want: indexer.DefaultContractLinkedSchedule()},
		{name: "file", cfg: config.ContractAnalysisConfig{LinkedPolicy: "throttled", LinkedThrottleInterval: "1m"}, want: indexer.ContractLinkedSchedule{Policy: indexer.ContractLinkedThrottled, IdleWindow: 5 * time.Second, ThrottleInterval: time.Minute}},
		{name: "environment", cfg: config.ContractAnalysisConfig{LinkedPolicy: "throttled"}, env: "eager", want: indexer.ContractLinkedSchedule{Policy: indexer.ContractLinkedEager, IdleWindow: 5 * time.Second, ThrottleInterval: 30 * time.Second}},
		{name: "typo", cfg: config.ContractAnalysisConfig{LinkedPolicy: "idel"}, want: indexer.DefaultContractLinkedSchedule(), warn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			got := contractLinkedSchedule(tc.cfg, func(name string) string {
				if name == indexer.ContractLinkedPolicyEnv {
					return tc.env
				}
				return ""
			}, zap.New(core))
			require.Equal(t, tc.want, got)
			require.Len(t, logs.FilterMessage("contract analysis linked policy setting ignored").All(), map[bool]int{true: 1}[tc.warn])
			lines := logs.FilterMessage("contract analysis linked policy").All()
			require.Len(t, lines, 1)
			require.Equal(t, string(tc.want.Policy), lines[0].ContextMap()["policy"])
		})
	}
}
