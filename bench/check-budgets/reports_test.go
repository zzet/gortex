package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadP95MissingTimingIsNotZero(t *testing.T) {
	for _, value := range []*float64{nil, pointer(-1)} {
		p := phase{Attempts: []attempt{{Role: "workload", Outcome: "success", LogicalMS: value}}}
		if _, err := readP95(p); err == nil {
			t.Fatal("missing/invalid timing became a passing zero")
		}
	}
	if got, err := readP95(phase{Attempts: []attempt{{Role: "workload", Outcome: "success", LogicalMS: pointer(0)}}}); err != nil || got != 0 {
		t.Fatalf("explicit measured zero must remain distinguishable: %g %v", got, err)
	}
}

func TestRepeatedSeedsMayVaryButMatchedRolesMustAgree(t *testing.T) {
	a := report{Plan: strings.Repeat("a", 64), Workload: json.RawMessage(`{"scenario":"read","agents":32,"seed":1}`)}
	b := a
	b.Plan, b.Workload = strings.Repeat("b", 64), json.RawMessage(`{"scenario":"read","agents":32,"seed":2}`)
	if comparable(a, b) || !comparableRepeats(a, b) {
		t.Fatal("varying repetition seeds must be allowed only across repeats")
	}
	b.Workload = json.RawMessage(`{"scenario":"read","agents":8,"seed":1}`)
	if !comparableReadLoads(a, b) || comparableRepeats(a, b) {
		t.Fatal("load32/load8 allows only the intentional agent-count difference")
	}
	b.Workload = json.RawMessage(`{"scenario":"read","agents":8,"seed":2}`)
	if comparableReadLoads(a, b) {
		t.Fatal("load32/load8 reference must use the same matching-repeat seed")
	}
	b.Workload = json.RawMessage(`{"scenario":"read","agents":32,"seed":9007199254740993}`)
	a.Workload = json.RawMessage(`{"scenario":"read","agents":32,"seed":9007199254740992}`)
	a.Plan = b.Plan
	if comparable(a, b) {
		t.Fatal("large distinct integer seeds must not round together through float64")
	}
}

func syntheticReport() map[string]any {
	value := func(n float64, unit string) map[string]any {
		return map[string]any{"value": n, "unit": unit, "reason": ""}
	}
	return map[string]any{
		"schema_version": 1, "run_id": "synthetic-repeat", "git_sha": strings.Repeat("a", 40), "profile": "agent", "seed": 1, "request_plan_sha256": strings.Repeat("b", 64), "complete": true, "errors": nil,
		"crashes":  map[string]any{"panic_lines": 0, "sigabrt_lines": 0, "oom_log_lines": 0},
		"workload": map[string]any{"scenario": "read", "agents": 1, "edit_agents": 0, "linked_worktrees": 0, "seed": 1, "profile": "agent", "duration_seconds": 120, "product_deadline_seconds": 120, "session_model": "per_agent"},
		"host": map[string]any{"machine": "synthetic-host", "os": "linux", "arch": "arm64", "kernel": "test", "profile": "reference", "validated": true, "limits": map[string]any{
			"cpu_quota": value(8, "cores"), "memory_max": value(16*1024*1024*1024, "bytes"), "logical_cpus": value(8, "cores"), "host_memory": value(16*1024*1024*1024, "bytes"), "disk_capacity_bytes": value(60*1024*1024*1024, "bytes"),
		}},
		"corpus": []any{map[string]any{"label": "gortex", "commit": strings.Repeat("c", 40)}},
		"phases": []any{map[string]any{
			"name": "read", "session_model": "per_agent", "duration_seconds": 120, "never_visible": 0, "visibility_censored": 0, "errors": nil,
			"metrics":  map[string]any{"force_killed": value(0, "boolean")},
			"gate":     map[string]any{"readiness": map[string]any{"ready": true, "enriched": true, "loaded_roots": []string{"/owned/synthetic"}}},
			"tools":    map[string]any{"search.symbols": map[string]any{"errors": map[string]any{}, "phase_censored": 0, "product_expired": 0}},
			"attempts": []any{map[string]any{"tool": "search.symbols", "role": "workload", "outcome": "success", "code": 0, "logical_ms": 100}},
		}},
	}
}

func TestPairedChildCannotMasqueradeAsStandaloneCapacity(t *testing.T) {
	cfg := inputs{CandidateRevision: strings.Repeat("a", 40), Hosts: map[string]string{"reference_linux": "synthetic-host"}}
	input := runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "load-1"}
	for _, mutation := range []string{"paired-window", "null-paired-window", "coupled-phase", "coupled-workload"} {
		t.Run(mutation, func(t *testing.T) {
			raw := syntheticReport()
			p := raw["phases"].([]any)[0].(map[string]any)
			switch mutation {
			case "paired-window":
				p["gate"].(map[string]any)["paired_measurement_window"] = map[string]any{"group": map[string]any{"method": "coupled native back-to-back requests"}}
			case "null-paired-window":
				p["gate"].(map[string]any)["paired_measurement_window"] = nil
			case "coupled-phase":
				p["session_model"] = "coupled_native"
			case "coupled-workload":
				raw["workload"].(map[string]any)["session_model"] = "coupled_native"
			}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r, err := decodeReport(data)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateReport(cfg, input, r); err == nil {
				t.Fatal("coupled child accepted after envelope removed")
			}
		})
	}
}

func TestReportZeroCriticalEvidenceCannotBeOmittedOrNull(t *testing.T) {
	cfg := inputs{CandidateRevision: strings.Repeat("a", 40), Hosts: map[string]string{"reference_linux": "synthetic-host"}}
	input := runInput{Role: "candidate", HostClass: "reference_linux", Scenario: "load-1"}
	validate := func(raw map[string]any) error {
		data, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		r, err := decodeReport(data)
		if err != nil {
			return err
		}
		return validateReport(cfg, input, r)
	}
	if err := validate(syntheticReport()); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"never_visible", "visibility_censored"} {
		for _, null := range []bool{false, true} {
			raw := syntheticReport()
			p := raw["phases"].([]any)[0].(map[string]any)
			delete(p, field)
			if null {
				p[field] = nil
			}
			if err := validate(raw); err == nil {
				t.Fatalf("missing/null %s passed", field)
			}
		}
	}
	for _, field := range []string{"logical_ms", "code"} {
		raw := syntheticReport()
		a := raw["phases"].([]any)[0].(map[string]any)["attempts"].([]any)[0].(map[string]any)
		delete(a, field)
		if err := validate(raw); err == nil {
			t.Fatalf("missing attempt %s passed", field)
		}
	}
	for _, field := range []string{"phase_censored", "product_expired", "errors"} {
		raw := syntheticReport()
		tool := raw["phases"].([]any)[0].(map[string]any)["tools"].(map[string]any)["search.symbols"].(map[string]any)
		delete(tool, field)
		if err := validate(raw); err == nil {
			t.Fatalf("missing tool %s passed", field)
		}
	}
	raw := syntheticReport()
	raw["crashes"].(map[string]any)["panic_lines"] = nil
	if err := validate(raw); err == nil {
		t.Fatal("null panic count passed")
	}
	raw = syntheticReport()
	raw["request_plan_sha256"] = strings.Repeat("z", 64)
	if err := validate(raw); err == nil {
		t.Fatal("non-hex request identity passed")
	}
}

func TestCheckerLoadsMatchedRepeatsWithDifferentSeeds(t *testing.T) {
	dir := t.TempDir()
	cfg := inputs{Version: 1, Repeats: 3, CandidateRevision: strings.Repeat("a", 40), MainRevision: strings.Repeat("d", 40), TagRevision: shippedRevision, Hosts: map[string]string{"reference_linux": "synthetic-host"}}
	write := func(path string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"candidate", "main"} {
		for repeat := 1; repeat <= 3; repeat++ {
			raw := syntheticReport()
			raw["run_id"], raw["seed"] = fmt.Sprintf("%s-%d", role, repeat), repeat
			raw["workload"].(map[string]any)["seed"] = repeat
			raw["request_plan_sha256"] = fmt.Sprintf("%064x", repeat)
			if role == "main" {
				raw["git_sha"] = cfg.MainRevision
			}
			file := fmt.Sprintf("%s-%d.json", role, repeat)
			write(filepath.Join(dir, file), raw)
			cfg.Runs = append(cfg.Runs, runInput{Role: role, HostClass: "reference_linux", Scenario: "load-1", Repeat: repeat, Report: file})
		}
	}
	manifest := filepath.Join(dir, "inputs.json")
	write(manifest, cfg)
	d, err := loadInputs(manifest)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPolicy([]byte(strings.ReplaceAll(strings.ReplaceAll(policyFixture, "edit_ack_p95", "read_p95"), "loadedit-4", "load-1")))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := check(p, d, "release")
	if err != nil || len(rows) != 1 || !rows[0].Pass {
		t.Fatalf("matching per-repeat seeds should pass: %+v %v", rows, err)
	}
	// A mismatched seed on the second (not first) repetition must be refused.
	for i := range d.runs {
		if d.runs[i].input.Role == "main" && d.runs[i].input.Repeat == 2 {
			d.runs[i].data.Plan = strings.Repeat("e", 64)
		}
	}
	rows, err = check(p, d, "release")
	if err != nil || rows[0].Pass {
		t.Fatal("mismatched second-repeat plan passed")
	}
	raw := syntheticReport()
	raw["phases"].([]any)[0].(map[string]any)["attempts"].([]any)[0].(map[string]any)["logical_ms"] = nil
	write(filepath.Join(dir, "candidate-1.json"), raw)
	if _, err := loadInputs(manifest); err == nil {
		t.Fatal("missing timing passed the report-to-checker boundary")
	}
}
