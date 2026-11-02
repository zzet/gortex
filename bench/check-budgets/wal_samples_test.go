package main

import (
	"encoding/json"
	"testing"
	"time"
)

func walBytes(v float64) *float64 { return &v }

func TestWALP95UsesCompleteIndependentRawSampleCensus(t *testing.T) {
	start := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	wire := walSampleWire{Version: 1, Started: start, Finished: start.Add(time.Second), Count: 20}
	for i := range 20 {
		wire.Samples = append(wire.Samples, walSample{At: start.Add(time.Duration(i) * time.Millisecond), Bytes: walBytes(1024 * 1024)})
	}
	wire.Samples[19].Bytes = walBytes(40 * 1024 * 1024)
	raw, _ := json.Marshal(wire)
	value := float64(1024 * 1024)
	window, _ := json.Marshal(map[string]any{"started": wire.Started, "ended": wire.Finished})
	p := phase{Gate: map[string]json.RawMessage{"wal_samples_v1": raw, "process_counter_window": window}, Metrics: map[string]metric{"p95_wal_bytes": {Value: &value, Unit: "bytes"}}}
	got, err := measureWALP95(p)
	if err != nil || got != 1 {
		t.Fatalf("raw nearest-rank p95 got%v %v", got, err)
	}
	for _, mutate := range []func(*walSampleWire){
		func(w *walSampleWire) { w.Version = 2 },
		func(w *walSampleWire) { w.Count++ },
		func(w *walSampleWire) { w.Samples = nil; w.Count = 0 },
		func(w *walSampleWire) { w.Samples[0].Error = "absent" },
		func(w *walSampleWire) { w.Samples[0].Bytes = walBytes(-1) },
		func(w *walSampleWire) { w.Samples[0].At = w.Finished.Add(time.Second) },
	} {
		changed := wire
		changed.Samples = append([]walSample(nil), wire.Samples...)
		mutate(&changed)
		p.Gate["wal_samples_v1"], _ = json.Marshal(changed)
		if _, err := measureWALP95(p); err == nil {
			t.Fatal("invalid or incomplete raw census accepted")
		}
	}
	p.Gate["wal_samples_v1"] = raw
	p.Gate["process_counter_window"] = json.RawMessage(`{"started":"2000-01-01T00:00:00Z","ended":"2000-01-01T00:00:01Z"}`)
	if _, err := measureWALP95(p); err == nil {
		t.Fatal("foreign sample window accepted")
	}
	p.Gate["process_counter_window"] = window
	value++
	if _, err := measureWALP95(p); err == nil {
		t.Fatal("aggregate/raw disagreement accepted")
	}
}

func TestWALP95MissingAndNullRemainUnavailable(t *testing.T) {
	for _, raw := range []string{"null", `{}`, `{"schema_version":1,"samples":null}`, `{"schema_version":1,"foreign":true}`} {
		if _, err := measureWALP95(phase{Gate: map[string]json.RawMessage{"wal_samples_v1": json.RawMessage(raw)}}); err == nil {
			t.Fatal("missing raw observations became zero")
		}
	}
}
