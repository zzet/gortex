package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMixedBackgroundOverlayPinsAndPreservesStrictReport(t *testing.T) {
	for _, mode := range []string{"qualified", "wrong_report_hash", "wrong_status_hash", "duplicate_status", "extra_runtime_error", "extra_edit_failure", "changed_physical", "existing_output", "crash_flag", "missing_crash_census", "unknown_crash_census"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			wire, status := mixedBackgroundFixture(t)
			if mode == "extra_edit_failure" {
				wire.Edits[1].Outcome = "phase_censored"
			}
			if mode == "changed_physical" {
				status = bytes.Replace(status, []byte(`"patch_wall_ms":15`), []byte(`"patch_wall_ms":30`), 1)
			}
			r := editLoopEvidenceFixture(t)
			r.Complete = false
			if mode == "crash_flag" {
				v := 1
				r.Crashes["panic_lines"] = &v
			}
			if mode == "missing_crash_census" {
				delete(r.Crashes, "panic_lines")
			}
			if mode == "unknown_crash_census" {
				v := 0
				r.Crashes["unrecognized"] = &v
			}
			errorText := "edit 0: diagnostic_failure (" + strings.Join(wire.Edits[0].Errors, "; ") + ")"
			r.Errors = []string{"edit-and-probe: " + errorText}
			r.Phases[0].Errors = []string{errorText}
			if mode == "extra_runtime_error" {
				r.Errors = append(r.Errors, "stop timeout")
			}
			encoded, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = encoded
			rpc := map[string]any{"event": "rpc", "purpose": "status", "tool": "change.receipt", "wire_id": 1, "payload": map[string]any{"content": []any{map[string]any{"type": "text", "text": string(status)}}}}
			rawStatus, err := json.Marshal(rpc)
			if err != nil {
				t.Fatal(err)
			}
			wire.Edits[0].EvidenceBytes = len(rawStatus)
			encoded, err = json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = encoded
			reportBytes, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			reportPath := filepath.Join(dir, "raw.json")
			statusPath := filepath.Join(dir, "status.json")
			if err = os.WriteFile(reportPath, reportBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(statusPath, rawStatus, 0o600); err != nil {
				t.Fatal(err)
			}
			pin := map[string]any{"ordinal": 0, "path": "status.json", "sha256": fmt.Sprintf("%x", sha256.Sum256(rawStatus))}
			pins := map[string]any{"schema_version": 1, "report_sha256": fmt.Sprintf("%x", sha256.Sum256(reportBytes)), "statuses": []any{pin}}
			switch mode {
			case "wrong_report_hash":
				pins["report_sha256"] = strings.Repeat("0", 64)
			case "wrong_status_hash":
				pin["sha256"] = strings.Repeat("0", 64)
			case "duplicate_status":
				pins["statuses"] = []any{pin, pin}
			}
			data, err := json.Marshal(pins)
			if err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(dir, "pins.json")
			if err = os.WriteFile(manifest, data, 0o600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "overlay.json")
			if mode == "existing_output" {
				if err = os.WriteFile(output, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"edit-qualify", "--report", reportPath, "--statuses", manifest, "--out", output}, &stdout, &stderr)
			if (code == 0) != (mode == "qualified") {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			after, err := os.ReadFile(reportPath)
			if err != nil || !bytes.Equal(after, reportBytes) {
				t.Fatal("original report changed", err)
			}
			if mode == "qualified" {
				var result editQualificationResult
				b, readErr := os.ReadFile(output)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if err = json.Unmarshal(b, &result); err != nil {
					t.Fatal(err)
				}
				if result.Policy != mixedBackgroundPolicy || result.RawComplete || result.AggregateAcceptance || len(result.Qualifications) != 1 || len(result.RawErrors) != 1 || result.Qualifications[0].Background.Value != nil || result.Qualifications[0].RawOutcome != "diagnostic_failure" || len(result.Qualifications[0].OriginalErrors) != 1 {
					t.Fatalf("dishonest overlay: %+v", result)
				}
			}
		})
	}
}
