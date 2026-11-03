package main

import (
	"encoding/json"
	"testing"
	"time"
)

func readGateFixture(t *testing.T) phase {
	t.Helper()
	start := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	captures := statusCaptureWire{Version: 1, Count: 2, Captures: []statusCapture{{Index: 0, PID: 123, Started: start, Finished: start.Add(time.Millisecond)}, {Index: 1, PID: 123, Started: start.Add(time.Second), Finished: start.Add(time.Second + time.Millisecond)}}}
	raw, _ := json.Marshal(captures)
	window, _ := json.Marshal(map[string]any{"started": start.Add(2 * time.Millisecond), "ended": start.Add(900 * time.Millisecond)})
	value := float64(2)
	return phase{Gate: map[string]json.RawMessage{"status_capture_v1": raw, "process_counter_window": window}, Status: []json.RawMessage{json.RawMessage(`{"pid":123,"storage":{"wal_reclaim":{"pause_count":7}}}`), json.RawMessage(`{"pid":123,"storage":{"wal_reclaim":{"pause_count":9}}}`)}, Metrics: map[string]metric{"reclaim_gate_closures": {Value: &value, Unit: "closures"}}}
}

func TestReadGateCounterRequiresActualSameDaemonCensus(t *testing.T) {
	p := readGateFixture(t)
	value, err := measureReadGateClosures(p)
	if err != nil || value != 2 {
		t.Fatalf("actual endpoint delta: %v %v", value, err)
	}
	for _, raw := range []string{`null`, `{}`, `{"pid":456,"storage":{"wal_reclaim":{"pause_count":9}}}`, `{"pid":123,"storage":{"wal_reclaim":{"pause_count":null}}}`, `{"pid":123,"storage":{"wal_reclaim":{"pause_count":1}}}`, `{"pid":123,"storage":{"wal_reclaim":{"pause_count":9.5}}}`} {
		p := readGateFixture(t)
		p.Status[1] = json.RawMessage(raw)
		if _, err := measureReadGateClosures(p); err == nil {
			t.Fatal("missing/foreign/reset/fractional counter accepted")
		}
	}
}

func TestReadGateCounterRejectsTruncatedAndForeignWindow(t *testing.T) {
	for _, change := range []func(*phase){
		func(p *phase) { p.Status = p.Status[:1] },
		func(p *phase) { p.Gate["status_capture_v1"] = json.RawMessage(`{"schema_version":2}`) },
		func(p *phase) {
			p.Gate["process_counter_window"] = json.RawMessage(`{"started":"2000-01-01T00:00:00Z","ended":"2000-01-01T00:00:01Z"}`)
		},
		func(p *phase) {
			v := float64(0)
			p.Metrics["reclaim_gate_closures"] = metric{Value: &v, Unit: "closures"}
		},
	} {
		p := readGateFixture(t)
		change(&p)
		if _, err := measureReadGateClosures(p); err == nil {
			t.Fatal("incomplete or foreign counter census accepted")
		}
	}
}

// Exercise the production extract switch, not just the adapter seam. Older
// shipped daemons without the capability remain unavailable, never zero.
func TestReadGateMetricAdmissionRequiresRawCapability(t *testing.T) {
	if metricSpecs["read_gate_closures"].missing != "" {
		t.Fatal("qualified raw counter adapter is not connected")
	}
	p := readGateFixture(t)
	value, err := extract("read_gate_closures", "reference_linux", report{Phases: []phase{p}})
	if err != nil || value != 2 {
		t.Fatalf("wired counter extract: %v %v", value, err)
	}
	for _, change := range []func(*phase){
		func(p *phase) { p.Status[1] = json.RawMessage(`{"pid":123,"storage":{}}`) },
		func(p *phase) { p.Status[1] = json.RawMessage(`{"storage":{"wal_reclaim":{"pause_count":9}}}`) },
		func(p *phase) { delete(p.Gate, "status_capture_v1") },
		func(p *phase) { delete(p.Gate, "process_counter_window") },
	} {
		p := readGateFixture(t)
		change(&p)
		if _, err := extract("read_gate_closures", "reference_linux", report{Phases: []phase{p}}); err == nil {
			t.Fatal("older/foreign capability became supported zero")
		}
	}
}
