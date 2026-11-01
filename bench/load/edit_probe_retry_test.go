package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type editProbeRetryStub struct{ calls int }

func (s *editProbeRetryStub) call(context.Context, string, any) (json.RawMessage, error) {
	s.calls++
	if s.calls == 1 {
		// Actual cmd/gortex/daemon_mcp_repo_loading.go JSON-RPC error shape,
		// not a synthetic top-level retryable property.
		var reply wireReply
		if err := json.Unmarshal([]byte(`{"id":1,"error":{"code":-32000,"message":"repository loading","data":{"error_code":"repo_loading","retryable":true}}}`), &reply); err != nil {
			return nil, err
		}
		return nil, reply.Error
	}
	return json.RawMessage(`{"content":[{"type":"text","text":"{\"ok\":true}"}]}`), nil
}

func TestEditProbeRefusalClassificationUsesActualWireFields(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		retry      bool
	}{
		{"busy", `{"id":1,"error":{"code":-32002,"message":"busy","data":{"outcome":"server_busy","retryable":true,"delivery_unknown":false}}}`, true},
		{"loading", `{"id":1,"error":{"code":-32000,"message":"loading","data":{"error_code":"repo_loading","retryable":true}}}`, true},
		{"unknown_delivery", `{"id":1,"error":{"code":-32002,"message":"busy","data":{"retryable":true,"delivery_unknown":true}}}`, false},
		{"unknown_code", `{"id":1,"error":{"code":-32000,"message":"unknown","data":{"error_code":"unknown","retryable":true}}}`, false},
		{"wrong_level", `{"id":1,"error":{"code":-32000,"message":"loading","retryable":true}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reply wireReply
			if err := json.Unmarshal([]byte(tc.wire), &reply); err != nil {
				t.Fatal(err)
			}
			if reply.Error == nil || reply.Error.retryEditProbe() != tc.retry {
				t.Fatalf("classification mismatch%+v", reply.Error)
			}
		})
	}
	blocked := json.RawMessage(`{"isError":true,"content":[{"text":"{\"error_code\":\"tool_blocked_by_mode\",\"message\":\"switch mode\",\"retriable\":true}"}]}`)
	err := checkToolResult(blocked)
	remote, ok := err.(*rpcError)
	if !ok || remote.retryEditProbe() {
		t.Fatalf("action-required guidance became automatic retry%v", err)
	}
}

func TestEditProbeTypedRetryPolicyDoesNotChangeLegacyCalls(t *testing.T) {
	r := request{Tool: "edit", Operation: "file"}
	sleep := func(context.Context, time.Duration) error { return nil }
	legacy := &editProbeRetryStub{}
	_, rows, err := callTool(t.Context(), legacy, r, nil, "workload", 0, 0, 0, time.Second, sleep)
	if err == nil || legacy.calls != 1 || len(rows) != 1 || rows[0].Outcome != "error" {
		t.Fatalf("legacy retry behavior changed: %+v %v", rows, err)
	}
	scoped := &editProbeRetryStub{}
	_, rows, err = callEditProbeTool(t.Context(), scoped, r, nil, "workload", 0, 0, 0, time.Second, sleep)
	if err != nil || scoped.calls != 2 || len(rows) != 2 || rows[0].Outcome != "retryable" || rows[1].Outcome != "success" {
		t.Fatalf("typed probe retry missing: %+v %v", rows, err)
	}
	metrics := summarize(rows, 1, "workload")["edit.file"]
	if metrics.RetryableRefusals != 1 || metrics.BusyRefusals != 0 || metrics.Successes != 1 || len(metrics.Errors) != 0 {
		t.Fatalf("retry refusal misclassified: %+v", metrics)
	}
}
