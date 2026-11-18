//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"strings"
	"testing"
	"time"
)

func projectionFakeStat(pid, birth string) string {
	return pid + " (daemon (worker)) S " + strings.Repeat("0 ", 18) + birth + " 0\n"
}

type projectionBookendCaller struct {
	calls  int
	method string
	params any
}

func (c *projectionBookendCaller) callWithIdentity(_ context.Context, method string, params any) (json.RawMessage, wireCallIdentity, error) {
	c.calls++
	c.method, c.params = method, params
	now := time.Now()
	raw := json.RawMessage(`{"contents":[{"uri":"gortex://index-health","text":"{\"pending_projection\":{\"projection_divergence\":0}}"}]}`)
	return raw, wireCallIdentity{ID: 1, Started: now, WriteStarted: now, WriteFinished: now, ReplyDelivered: now, Finished: now}, nil
}

func TestProjectionBookendOptInAndMissingOwnershipContract(t *testing.T) {
	if flag.Lookup("readback-reference-projection-evidence").DefValue != "false" {
		t.Fatal("default rows changed")
	}
	caller := &projectionBookendCaller{}
	got := collectProjectionBookend(context.Background(), caller, -1, "before", nil)
	if caller.calls != 1 || caller.method != "resources/read" {
		t.Fatal("new tool shape or hidden retry")
	}
	params, ok := caller.params.(map[string]any)
	if !ok || len(params) != 1 || params["uri"] != "gortex://index-health" {
		t.Fatal("resource query shape changed")
	}
	if got.Process.Peak.Value != nil || got.Counter.Value != nil || got.RPC.ID != 1 || len(got.RPC.Raw) == 0 {
		t.Fatal("missing owner became a measured zero or raw proof was discarded")
	}
}

func TestProjectionProcessBirthContract(t *testing.T) {
	for _, tc := range []struct {
		name, stat string
		pid        int
		valid      bool
	}{
		{"nested_comm", projectionFakeStat("42", "900"), 42, true},
		{"wrong_pid", projectionFakeStat("43", "900"), 42, false},
		{"missing", "42 (daemon) S 0", 42, false},
		{"zero_birth", projectionFakeStat("42", "0"), 42, false},
		{"negative_birth", projectionFakeStat("42", "-1"), 42, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			birth, err := projectionBirth(tc.stat, tc.pid)
			if (err == nil) != tc.valid || tc.valid && birth != "900" {
				t.Fatalf("birth=%q err=%v", birth, err)
			}
		})
	}
}

func TestProjectionHWMContract(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
		bytes float64
	}{
		{"VmHWM:\t123 kB\n", true, 123 * 1024},
		{"VmHWM: 0 kB\n", true, 0},
		{"VmRSS: 123 kB\n", false, 0},
		{"VmHWM: 123 bytes\n", false, 0},
		{"VmHWM: -1 kB\n", false, 0},
		{"VmHWM: 1.5 kB\n", false, 0},
		{"VmHWM: 1 kB\nVmHWM: 2 kB\n", false, 0},
		{"VmHWM: 99999999999999999999 kB\n", false, 0},
	} {
		value, err := projectionHWM(tc.raw)
		if (err == nil) != tc.valid || tc.valid && value != tc.bytes {
			t.Fatalf("raw=%q value=%v err=%v", tc.raw, value, err)
		}
	}
}

func TestProjectionOwnedSnapshotRejectsPIDReuseAndMissingData(t *testing.T) {
	for _, mode := range []string{"valid", "reused", "missing_status", "missing_boot"} {
		t.Run(mode, func(t *testing.T) {
			statReads := 0
			read := func(path string) ([]byte, error) {
				switch {
				case strings.HasSuffix(path, "/stat"):
					statReads++
					birth := "900"
					if mode == "reused" && statReads == 2 {
						birth = "901"
					}
					return []byte(projectionFakeStat("42", birth)), nil
				case strings.HasSuffix(path, "/status"):
					if mode == "missing_status" {
						return nil, errors.New("missing")
					}
					return []byte("VmHWM: 123 kB\n"), nil
				default:
					if mode == "missing_boot" {
						return nil, errors.New("missing")
					}
					return []byte("boot-epoch\n"), nil
				}
			}
			got := projectionProcessSnapshot(42, read)
			if (got.Peak.Value != nil) != (mode == "valid") {
				t.Fatalf("mode=%s evidence=%+v", mode, got)
			}
			if mode == "valid" && (*got.Peak.Value != 123*1024 || got.BirthTicks != "900" || got.BootID != "boot-epoch") {
				t.Fatal("wrong bound metric")
			}
		})
	}
}

func TestProjectionCounterMissingIsNotZero(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
		value float64
	}{
		{`{"pending_projection":{"projection_divergence":0}}`, true, 0},
		{`{"pending_projection":{"projection_divergence":7}}`, true, 7},
		{`{}`, false, 0},
		{`{"pending_projection":null}`, false, 0},
		{`{"pending_projection":{}}`, false, 0},
		{`{"pending_projection":{"projection_divergence":null}}`, false, 0},
		{`{"pending_projection":{"projection_divergence":"0"}}`, false, 0},
		{`{"pending_projection":{"projection_divergence":-1}}`, false, 0},
		{`{"pending_projection":{"projection_divergence":0.5}}`, false, 0},
		{`{"pending_projection":{"projection_divergence":true}}`, false, 0},
		{`{"pending_projection":{"projection_divergence":9007199254740992}}`, false, 0},
	} {
		raw, _ := json.Marshal(map[string]any{"contents": []map[string]string{{"uri": "gortex://index-health", "text": tc.body}}})
		got := projectionCounter(raw)
		if (got.Value != nil) != tc.valid || tc.valid && *got.Value != tc.value {
			t.Fatalf("body=%s metric=%+v", tc.body, got)
		}
	}
}
