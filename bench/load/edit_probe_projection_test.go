package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEditProbeProjectionBookendNeverInfersZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		refuse  bool
		value   *float64
		section bool
	}{
		{name: "zero", body: `{"status":"ok","pending_projection":{"enabled":true,"published":4,"retired":4,"projection_divergence":0}}`, value: new(0.0), section: true},
		{name: "nonzero", body: `{"pending_projection":{"enabled":true,"projection_divergence":3}}`, value: new(3.0), section: true},
		{name: "section_absent", body: `{"status":"ok"}`},
		{name: "section_null", body: `{"pending_projection":null}`},
		{name: "resource_refused", refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := editProbeCorpus()
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.File), []byte("package fixture\n"+c.EditAnchor+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			run := newEditProbeStubRun(t, ctx, c, plan, dir, 0, func(frame stubFrame) (any, *rpcError) {
				if frame.Method != "resources/read" || frame.Params["uri"] != "gortex://index-health" {
					return stubContent(map[string]any{}), nil
				}
				calls++
				if tc.refuse {
					return nil, &rpcError{Code: -32603, Message: "resource unavailable"}
				}
				return map[string]any{"contents": []any{map[string]any{"uri": "gortex://index-health", "text": tc.body}}}, nil
			})
			if got := run.phase.EditProbe.ProjectionBefore.Divergence; got.Value != nil || got.Unit != "count" || got.Reason == "" {
				t.Fatalf("an uncollected bookend is not unavailable: %+v", got)
			}
			got := run.projectionBookend(ctx)
			if calls != 1 {
				t.Fatalf("index-health calls = %d, want exactly one attempt", calls)
			}
			if (got.Divergence.Value == nil) != (tc.value == nil) || tc.value != nil && *got.Divergence.Value != *tc.value {
				t.Fatalf("divergence = %+v, want %v", got.Divergence, tc.value)
			}
			if got.Divergence.Value == nil && (got.Divergence.Unit != "count" || got.Divergence.Reason == "") {
				t.Fatalf("a missing counter lost its unavailable reason: %+v", got.Divergence)
			}
			if (len(got.Section) != 0) != tc.section || got.ObservedAt == nil || got.WireID <= 0 {
				t.Fatalf("raw section or wire identity not retained: %+v", got)
			}
			if tc.refuse != (got.Error != "") {
				t.Fatalf("call failure not retained: %q", got.Error)
			}
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if string(wire["projection_divergence"]) == "" {
				t.Fatalf("bookend wire shape lost the counter: %s", data)
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
