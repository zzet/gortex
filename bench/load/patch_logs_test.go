package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPatchWriteAggregationPreservesIntervalsAndExcludesNestedPhases(t *testing.T) {
	start := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	makeLine := func(message string, seconds int, duration int, file string) string {
		row := map[string]any{"msg": message, "ts": start.Add(time.Duration(seconds) * time.Second).Format(time.RFC3339Nano), "ms": duration, "file": file, "kind": "modify", "delta_class": "body", "wal_bytes_reset": false}
		for key := range patchCounterUnits {
			row[key] = 100
		}
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	lines := []string{
		makeLine("graph patch complete", 3, 2000, "first.go"),
		makeLine("graph patch complete", 4, 2000, "second.go"), // overlaps first
		makeLine("graph patch phase complete", 3, 1000, "nested.go"),
		makeLine("graph patch complete", 1, 2000, "boundary.go"),
		makeLine("graph patch complete", 15, 1000, "outside.go"),
	}
	root := t.TempDir()
	paths := map[string]string{"daemon.log": filepath.Join(root, "daemon.log"), "daemon-1.stderr.log": filepath.Join(root, "stderr.log")}
	if err := os.WriteFile(paths["daemon.log"], []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths["daemon-1.stderr.log"], []byte(lines[0]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	phase := Phase{Started: start, DurationSec: 10, Gate: make(map[string]json.RawMessage), Edits: []EditObservation{
		{Outcome: "visible", VisibilityMS: measured(20, "ms")}, {Outcome: "phase_censored"}, {Outcome: "edit_error"},
	}}
	if err := collectPatchWrites(paths, &phase); err != nil {
		t.Fatal(err)
	}
	var evidence patchWriteEvidence
	if err := json.Unmarshal(phase.Gate["patch_write_intervals"], &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Acknowledged != 2 || evidence.Visible != 1 || evidence.Contained != 2 || evidence.Boundary != 1 || evidence.Overlapping != 1 || evidence.DuplicateCopies != 1 || len(evidence.Events) != 4 {
		t.Fatalf("wrong interval/edit census: %+v", evidence)
	}
	for key := range patchCounterUnits {
		if value := evidence.Totals[key]; value.Value == nil || *value.Value != 200 {
			t.Fatalf("%s parent totals double-counted phases/boundaries: %+v", key, value)
		}
		if value := evidence.PerAcknowledged[key]; value.Value == nil || *value.Value != 100 {
			t.Fatalf("%s completed-edit denominator: %+v", key, value)
		}
	}
	if !evidence.Window.Started.Equal(start) || !evidence.Window.Ended.Equal(start.Add(10*time.Second)) || !strings.Contains(evidence.Method, "not exclusive") {
		t.Fatalf("measurement qualification/window missing: %+v", evidence)
	}
}

func TestPatchWriteMissingLegacyResetAndInvalidCountersRemainUnavailable(t *testing.T) {
	start := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	for _, record := range []string{
		`{"ts":1793059202,"msg":"graph patch complete","ms":100,"file":"old.go"}`,
		`{"ts":1793059202,"msg":"graph patch complete","ms":100,"file":"reset.go","wal_bytes":42,"wal_bytes_reset":true,"rows":null}`,
		`{"ts":1793059202,"msg":"graph patch complete","file":"no-duration.go"}`,
		`console graph patch complete no structured counters`,
		"",
	} {
		t.Run(record, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.log")
			if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
				t.Fatal(err)
			}
			phase := Phase{Started: start, DurationSec: 10, Gate: make(map[string]json.RawMessage), Edits: []EditObservation{{Outcome: "visible"}}}
			if err := collectPatchWrites(map[string]string{"daemon.log": path}, &phase); err != nil {
				t.Fatal(err)
			}
			var evidence patchWriteEvidence
			if err := json.Unmarshal(phase.Gate["patch_write_intervals"], &evidence); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"wal_bytes", "rows"} {
				if value := evidence.Totals[key]; value.Value != nil || value.Reason == "" {
					t.Fatalf("%s unavailable became zero: %+v", key, value)
				}
			}
		})
	}
}

func TestPatchWriteNumericTimestampAndZeroCounter(t *testing.T) {
	event, err := parsePatchWrite([]byte(`{"ts":1793059202.25,"msg":"graph patch complete","ms":250,"file":"source.go","rows":0,"txns":-1}`), "daemon.log")
	if err != nil {
		t.Fatal(err)
	}
	if !event.Ended.Equal(time.Unix(1793059202, 250000000)) || event.Ended.Sub(event.Started) != 250*time.Millisecond || event.Counters["rows"].Value == nil || *event.Counters["rows"].Value != 0 || event.Counters["txns"].Value != nil {
		t.Fatalf("timestamp or counter units lost: %+v", event)
	}
}
