package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditLoopGateRetainsEveryRowWhenEvidenceIsMissing(t *testing.T) {
	var out, diagnostics bytes.Buffer
	code := run([]string{"edit-loop", "--budgets", "../budgets.yaml", "--scenario", "loadedit-4", "--inputs", filepath.Join(t.TempDir(), "absent-inputs.json")}, &out, &diagnostics)
	if code != 1 {
		t.Fatalf("missing gate evidence exit=%d, want1; diagnostics=%s", code, diagnostics.String())
	}
	text := out.String()
	for _, row := range []string{"edit_ack_p95", "edit_ack_max", "readback_p95", "readback_max", "readback_correct", "silent_stale_json", "silent_stale_toon", "silent_stale_compact", "silent_stale_facade", "silent_stale_cli", "wal_p95_mib", "wal_peak_mib", "reclaim_hold_p95", "read_gate_closures"} {
		if !strings.Contains(text, row) {
			t.Errorf("missing evidence hid mandatory row %s: %s", row, text)
		}
	}
	if !strings.Contains(text, "UNAVAILABLE") || !strings.Contains(strings.ToLower(text), "baseline") || !strings.Contains(strings.ToLower(text), "delta") {
		t.Errorf("gate lost unavailable/baseline/delta interpretation: %s", text)
	}
	if strings.Contains(text, "\tPASS\t") {
		t.Fatal("unobserved row became a numeric pass")
	}
}
