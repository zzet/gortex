package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type nightlySeedCaller struct{ calls int }

func (c *nightlySeedCaller) call(context.Context, string, any) (json.RawMessage, error) {
	c.calls++
	return json.RawMessage(`{"content":[]}`), nil
}

func TestNightlySeedRealDelayInsideMeasuredCall(t *testing.T) {
	c := &nightlySeedCaller{}
	ctx := context.WithValue(context.Background(), nightlySeedKey{}, "search-delay-v1")
	_, rows, err := callTool(ctx, c, request{Tool: "search", Operation: "symbols"}, nil, "workload", 0, 0, 0, 3*time.Second, waitContext)
	if err != nil || c.calls != 1 || len(rows) != 1 || rows[0].DurationMS < 1000 || rows[0].NightlySeed != "search-delay-v1" || rows[0].NightlyDelayNS < int64(time.Second) {
		t.Fatalf("actual delay not retained inside measurement: %+v %v calls%d", rows, err, c.calls)
	}
}

func TestNightlySeedCancellationPreventsRPC(t *testing.T) {
	c := &nightlySeedCaller{}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), nightlySeedKey{}, "search-delay-v1"), 5*time.Millisecond)
	defer cancel()
	_, rows, err := callTool(ctx, c, request{Tool: "search", Operation: "symbols"}, nil, "workload", 0, 0, 0, time.Second, waitContext)
	if !errors.Is(err, context.DeadlineExceeded) || c.calls != 0 || len(rows) != 1 || rows[0].Outcome != "phase_censored" || rows[0].NightlyDelayNS <= 0 {
		t.Fatalf("cancellation lost: %+v %v calls%d", rows, err, c.calls)
	}
}

func TestNightlySeedNormalOffAndOtherToolsUnchanged(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), context.WithValue(context.Background(), nightlySeedKey{}, "search-delay-v1")} {
		c := &nightlySeedCaller{}
		_, rows, err := callTool(ctx, c, request{Tool: "get_symbol"}, nil, "workload", 0, 0, 0, time.Second, waitContext)
		if err != nil || c.calls != 1 || rows[0].NightlySeed != "" || rows[0].NightlyDelayNS != 0 {
			t.Fatalf("ordinary call changed: %+v %v", rows, err)
		}
		data, _ := json.Marshal(rows[0])
		var raw map[string]any
		_ = json.Unmarshal(data, &raw)
		if _, ok := raw["nightly_workload_seed"]; ok {
			t.Fatal("zero seed changed normal report shape")
		}
	}
	if err := validateNightlyWorkloadSeed(runOptions{NightlySeed: "unknown"}); err == nil {
		t.Fatal("unknown seed accepted")
	}
	if err := validateNightlyWorkloadSeed(runOptions{NightlySeed: "search-delay-v1", Scenario: "edit", Agents: 8}); err == nil {
		t.Fatal("unsupported scenario accepted")
	}
}
