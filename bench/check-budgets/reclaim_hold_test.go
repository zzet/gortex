package main

import (
	"encoding/json"
	"testing"
)

func holdStatus(t *testing.T, counts []int64, n int64, maximum float64) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"pid": 123, "storage": map[string]any{"wal_reclaim": map[string]any{"writer_hold": map[string]any{"count": n, "bucket_le_ms": []float64{10, 50, 100}, "bucket_counts": counts, "max_ms": maximum}}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func holdFixture(t *testing.T) phase {
	t.Helper()
	p := readGateFixture(t)
	// Independent population: before2holds; delta19<=10ms and one80ms.
	p.Status = []json.RawMessage{holdStatus(t, []int64{2, 0, 0, 0}, 2, 8), holdStatus(t, []int64{21, 0, 1, 0}, 22, 80)}
	v := float64(10)
	p.Metrics["reclaim_writer_hold_p95_ms"] = metric{Value: &v, Unit: "ms"}
	return p
}

func TestReclaimHoldRawHistogramBoundAndCensus(t *testing.T) {
	p := holdFixture(t)
	got, err := measureReclaimHoldP95(p)
	if err != nil || got != 0.01 {
		t.Fatalf("19of20<=10ms p95bound: %v %v", got, err)
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`{"pid":123,"storage":{}}`), holdStatus(t, []int64{21, 0, 1}, 22, 80), holdStatus(t, []int64{21, 0, 1, 0}, 23, 80), holdStatus(t, []int64{1, 0, 1, 0}, 2, 80), holdStatus(t, []int64{21, 0, 1, 0}, 22, 5)} {
		p := holdFixture(t)
		p.Status[1] = raw
		if _, err := measureReclaimHoldP95(p); err == nil {
			t.Fatal("missing/truncated/reset/inconsistent histogram accepted")
		}
	}
}

func TestReclaimHoldUnobservedAndForeignStayUnavailable(t *testing.T) {
	for _, change := range []func(*phase){
		func(p *phase) { p.Status[1] = p.Status[0] },
		func(p *phase) { delete(p.Gate, "status_capture_v1") },
		func(p *phase) { p.Status[1] = json.RawMessage(`{"pid":456}`) },
		func(p *phase) { delete(p.Metrics, "reclaim_writer_hold_p95_ms") },
	} {
		p := holdFixture(t)
		change(&p)
		if _, err := measureReclaimHoldP95(p); err == nil {
			t.Fatal("unknown histogram became zero")
		}
	}
}

func TestReclaimHoldMetricAdmissionAndExactRank(t *testing.T) {
	if metricSpecs["reclaim_hold_p95"].missing != "" {
		t.Fatal("qualified histogram adapter disconnected")
	}
	p := holdFixture(t)
	if v, err := extract("reclaim_hold_p95", "reference_linux", report{Phases: []phase{p}}); err != nil || v != 0.01 {
		t.Fatalf("wired histogram %v %v", v, err)
	}
	// ceil(95% of9007199254740990)=8556839292003941. A population
	// one below that rank in the first bucket must select the next bucket.
	p.Status[1] = holdStatus(t, []int64{8556839292003942, 450359962737050, 0, 0}, 9007199254740992, 50)
	v := float64(50)
	p.Metrics["reclaim_writer_hold_p95_ms"] = metric{Value: &v, Unit: "ms"}
	if v, err := measureReclaimHoldP95(p); err != nil || v != 0.05 {
		t.Fatalf("integer rank boundary %v %v", v, err)
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`{"pid":123,"storage":{}}`), json.RawMessage(`{"pid":123,"storage":{"wal_reclaim":{"writer_hold":null}}}`), json.RawMessage(`{"storage":{"wal_reclaim":{"writer_hold":{}}}}`)} {
		p := holdFixture(t)
		p.Status[1] = raw
		if _, err := extract("reclaim_hold_p95", "reference_linux", report{Phases: []phase{p}}); err == nil {
			t.Fatal("older or foreign histogram became supported zero")
		}
	}
}
