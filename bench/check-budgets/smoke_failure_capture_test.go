package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSmokeCaptureOptionReachesBothCollectionPaths(t *testing.T) {
	c := fixtureLaneConfig()
	off := smokeSequenceArgs(c, "/owned/runs")
	task := laneTask{Scenario: "loadedit-4", Role: "candidate", Repeat: 1, Seed: 1}
	offTask := taskArgs(c, task, "manifest", "scratch", "report")
	for _, args := range [][]string{off, offTask} {
		if slices.Contains(args, "--retain-read-source-failures") { t.Fatal("capture unexpectedly enabled by default") }
	}
	// JSON keeps this a semantic pre-fix RED: an old config ignores the new
	// optional field rather than failing to compile before argument generation.
	data, err := json.Marshal(c)
	if err != nil { t.Fatal(err) }
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil { t.Fatal(err) }
	raw["retain_read_source_failures"] = true
	data, err = json.Marshal(raw)
	if err != nil { t.Fatal(err) }
	if err := json.Unmarshal(data, &c); err != nil { t.Fatal(err) }
	for i, pair := range [][2][]string{{off, smokeSequenceArgs(c, "/owned/runs")}, {offTask, taskArgs(c, task, "manifest", "scratch", "report")}} {
		on := pair[1]
		if len(on) != len(pair[0])+1 || on[len(on)-1] != "--retain-read-source-failures" || !reflect.DeepEqual(pair[0], on[:len(on)-1]) {
			t.Fatalf("capture option was lost or changed the measured task: path=%d off=%v on=%v", i, pair[0], on)
		}
	}
}

func TestSmokeCaptureCLIStillRefusesUnownedWorkflow(t *testing.T) {
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	var output bytes.Buffer
	code := smokeDriverCommand([]string{"--retain-read-source-failures", "--candidate-binary", "candidate", "--load-binary", "load", "--main-bundle", "main", "--candidate-builds", "tests", "--fixture", "fixture", "--out", filepath.Join(t.TempDir(), "evidence")}, &output, &output)
	if code != 1 || !strings.Contains(output.String(), "explicit PR workflow") {
		t.Fatalf("opt-in flag did not reach the original workflow guard: exit=%d %s", code, &output)
	}
}

func TestSmokeCaptureAttestationMatchesAllActualReports(t *testing.T) {
	for _, mutation := range []string{"off", "on", "missing", "foreign-on", "records", "bytes", "encoding"} {
		t.Run(mutation, func(t *testing.T) {
			receipt, d, _ := syntheticSmokeJob(t)
			data, err := json.Marshal(receipt)
			if err != nil { t.Fatal(err) }
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil { t.Fatal(err) }
			raw["smoke_provenance"].(map[string]any)["retain_read_source_failures"] = mutation != "off" && mutation != "foreign-on"
			data, err = json.Marshal(raw)
			if err != nil { t.Fatal(err) }
			if err := json.Unmarshal(data, &receipt); err != nil { t.Fatal(err) }
			for i := range d.runs {
				data, err = json.Marshal(d.runs[i].data)
				if err != nil { t.Fatal(err) }
				raw = map[string]any{}
				if err := json.Unmarshal(data, &raw); err != nil { t.Fatal(err) }
				delete(raw, "read_source_failure_capture")
				if mutation != "off" && !(mutation == "missing" && i == 0) {
					cap := map[string]any{"max_records": 8, "max_combined_wire_bytes": 65536, "encoding": "base64"}
					if i == 0 { switch mutation { case "records": cap["max_records"] = 9; case "bytes": cap["max_combined_wire_bytes"] = 65537; case "encoding": cap["encoding"] = "utf8" } }
					raw["read_source_failure_capture"] = cap
				}
				data, err = json.Marshal(raw)
				if err != nil { t.Fatal(err) }
				if err := json.Unmarshal(data, &d.runs[i].data); err != nil { t.Fatal(err) }
			}
			err = validateSmokeJob(receipt, d)
			good := mutation == "off" || mutation == "on"
			if good && err != nil || !good && err == nil { t.Fatalf("capture config/attestation mismatch escaped strict checking: mutation=%s err=%v", mutation, err) }
		})
	}
}

func TestReportComparabilityIncludesCaptureOverhead(t *testing.T) {
	before, err := decodeReport(marshalSmokeCaptureControlJSON(t, syntheticReport()))
	if err != nil { t.Fatal(err) }
	raw := syntheticReport()
	raw["read_source_failure_capture"] = map[string]any{"max_records": 8, "max_combined_wire_bytes": 65536, "encoding": "base64"}
	after, err := decodeReport(marshalSmokeCaptureControlJSON(t, raw))
	if err != nil { t.Fatal(err) }
	if !comparable(before, before) || !comparable(after, after) || comparable(before, after) { t.Fatal("capture observation overhead was silently ignored in measurement identity") }
}

func TestSmokeWorkflowExposesExplicitDefaultOffCaptureOptIn(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "perf-gate.yml"))
	if err != nil { t.Fatal(err) }
	if !strings.Contains(string(data), `--retain-read-source-failures="${{ vars.PERF_PR_SMOKE_RETAIN_SOURCE_FAILURES == 'true' }}"`) {
		t.Fatal("existing smoke workflow cannot select bounded failure evidence without changing source again")
	}
}

func marshalSmokeCaptureControlJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil { t.Fatal(err) }
	return data
}
