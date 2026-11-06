package main

import (
	"testing"
	"time"
)

func sourcePendingReportFixture() map[string]any {
	raw := readFreshnessReportFixture("file_view_pending")
	p := raw["phases"].([]any)[0].(map[string]any)
	tools := p["tools"].(map[string]any)
	tools["read.source"] = tools["read.summary"]
	delete(tools, "read.summary")
	for _, row := range p["attempts"].([]any) {
		row.(map[string]any)["tool"] = "read.source"
	}
	p["attempts"].([]any)[0].(map[string]any)["error_data"].(map[string]any)["reason"] = "projection_deferred"
	return raw
}

func TestSourcePendingCheckerRequiresCompleteScopedHistory(t *testing.T) {
	if err := validateFreshnessFixture(t, sourcePendingReportFixture()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing_reason", "wrong_reason", "superseded", "missing_first", "missing_terminal", "counter_missing", "orphan_counter", "cost_reset", "probe", "write", "unknown_delivery", "cap", "wrong_source_marker"} {
		t.Run(name, func(t *testing.T) {
			raw := sourcePendingReportFixture()
			p := raw["phases"].([]any)[0].(map[string]any)
			rows := p["attempts"].([]any)
			first, last := rows[0].(map[string]any), rows[1].(map[string]any)
			data := first["error_data"].(map[string]any)
			metric := p["tools"].(map[string]any)["read.source"].(map[string]any)
			switch name {
			case "missing_reason":
				delete(data, "reason")
			case "wrong_reason":
				data["reason"] = "store_busy"
			case "superseded":
				data["error_code"] = "projection_superseded"
			case "missing_first":
				p["attempts"] = rows[1:]
			case "missing_terminal":
				p["attempts"] = rows[:1]
			case "counter_missing":
				delete(metric, "retryable_refusals")
			case "orphan_counter":
				p["attempts"] = []any{}
			case "cost_reset":
				last["logical_ms"] = 1
			case "probe":
				first["role"], last["role"] = "probe", "probe"
			case "write":
				first["tool"], last["tool"] = "edit.file", "edit.file"
			case "unknown_delivery":
				data["delivery_unknown"] = true
			case "cap":
				group := []any{first}
				for retry := 1; retry <= 2; retry++ {
					next := make(map[string]any)
					for key, value := range first {
						next[key] = value
					}
					next["retry"] = retry
					next["logical_ms"] = float64(2 + retry*3)
					next["started"] = first["started"].(time.Time).Add(time.Duration(retry*3) * time.Millisecond)
					group = append(group, next)
				}
				last["retry"] = 3
				metric["retryable_refusals"] = 3
				p["attempts"] = append(group, last)
			case "wrong_source_marker":
				last["read_metadata_state"] = "partial_pending"
				metric["partial_pending"] = 1
			}
			if err := validateFreshnessFixture(t, raw); err == nil {
				t.Fatal("source retry or partial-success evidence was broadened or lost")
			}
		})
	}
}
