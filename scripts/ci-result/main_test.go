package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This is one concrete, valid affected-PR receipt. Other scenarios alter the
// receipt and invoke the production CLI path, rather than reimplement its rules.
func affectedPRNeeds() map[string]ciNeed {
	return map[string]ciNeed{
		"plan": {Result: "success", Outputs: map[string]string{
			"mode": "affected", "has_tests": "true", "empty_ok": "false",
		}},
		"test":               {Result: "success"},
		"smoke":              {Result: "success"},
		"build-native":       {Result: "success"},
		"lint":               {Result: "success"},
		"build-linux-static": {Result: "success"},
		"build-onnx":         {Result: "success"},
		"benchmark":          {Result: "success"},
		"parser-stress":      {Result: "skipped"},
	}
}

func runCIGate(t *testing.T, event string, needs map[string]ciNeed, wantError string) {
	t.Helper()
	payload, err := json.Marshal(needs)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(string(payload), event, &stdout, &stderr)
	if wantError == "" {
		if code != 0 || !strings.Contains(stdout.String(), "All required CI checks passed") || stderr.Len() != 0 {
			t.Fatalf("gate rejected a valid receipt: exit=%d\nstdout=%s\nstderr=%s", code, &stdout, &stderr)
		}
		return
	}
	if code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "CI result: "+wantError) {
		t.Fatalf("gate must reject for %q: exit=%d\nstdout=%s\nstderr=%s", wantError, code, &stdout, &stderr)
	}
}

func TestCIResultGateAcceptsValidReceipts(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		outputs     map[string]string
		results     map[string]string
	}{
		{name: "affected PR", event: "pull_request"},
		{name: "explicit empty PR", event: "pull_request",
			outputs: map[string]string{"has_tests": "false", "empty_ok": "true"},
			results: map[string]string{"test": "skipped"}},
		{name: "full PR", event: "pull_request", outputs: map[string]string{"mode": "full"},
			results: map[string]string{"smoke": "skipped"}},
		{name: "full main", event: "push", outputs: map[string]string{"mode": "full"},
			results: map[string]string{"smoke": "skipped", "benchmark": "skipped"}},
		{name: "full manual", event: "workflow_dispatch", outputs: map[string]string{"mode": "full"},
			results: map[string]string{"smoke": "skipped", "benchmark": "skipped", "parser-stress": "success"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needs := affectedPRNeeds()
			for key, value := range tc.outputs {
				needs["plan"].Outputs[key] = value
			}
			for job, result := range tc.results {
				needs[job] = ciNeed{Result: result}
			}
			runCIGate(t, tc.event, needs, "")
		})
	}
}

func TestCIResultGateRejectsFailedCancelledOrUnexpectedlySkippedJobs(t *testing.T) {
	for job, receipt := range affectedPRNeeds() {
		for _, result := range []string{"failure", "cancelled", "skipped"} {
			if result == receipt.Result {
				continue // parser-stress is legitimately skipped for PRs.
			}
			t.Run(fmt.Sprintf("%s/%s", job, result), func(t *testing.T) {
				needs := affectedPRNeeds()
				changed := needs[job]
				changed.Result = result
				needs[job] = changed
				runCIGate(t, "pull_request", needs, job+" must be ")
			})
		}
	}
}

func TestCIResultGateRejectsInvalidPlanningOutputs(t *testing.T) {
	for _, tc := range []struct {
		name, wantError string
		outputs         map[string]string
	}{
		{name: "unvalidated empty", wantError: "empty test selection was not explicitly validated",
			outputs: map[string]string{"mode": "affected", "has_tests": "false", "empty_ok": "false"}},
		{name: "missing outputs", wantError: "planning outputs are missing or invalid", outputs: map[string]string{}},
		{name: "invalid has_tests", wantError: "planning outputs are missing or invalid",
			outputs: map[string]string{"mode": "affected", "has_tests": "oops"}},
		{name: "empty full suite", wantError: "empty test selection was not explicitly validated",
			outputs: map[string]string{"mode": "full", "has_tests": "false", "empty_ok": "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needs := affectedPRNeeds()
			needs["plan"] = ciNeed{Result: "success", Outputs: tc.outputs}
			if tc.outputs["has_tests"] == "false" {
				needs["test"] = ciNeed{Result: "skipped"}
			}
			runCIGate(t, "pull_request", needs, tc.wantError)
		})
	}
}

func TestCIWorkflowWiresSelectionBuildsCoverageAndStableGate(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	lf := strings.ReplaceAll(string(data), "\r\n", "\n")
	_, resultJob, found := strings.Cut(lf, "  ci-result:\n")
	if !found || !strings.Contains(resultJob, "uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e") ||
		!strings.Contains(resultJob, "go-version-file: go.mod") {
		t.Fatal("the final gate must install the repository's pinned Go toolchain")
	}
	for _, tc := range []struct{ name, text string }{
		{"LF", lf},
		{"CRLF", strings.ReplaceAll(lf, "\n", "\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) { assertCIWorkflowWiring(t, tc.text) })
	}
}

func assertCIWorkflowWiring(t *testing.T, input string) {
	t.Helper()
	// Git may check the workflow out with core.autocrlf on Windows. Match
	// Python read_text's universal-newline behavior on both checkout forms.
	text := strings.ReplaceAll(input, "\r\n", "\n")
	if strings.Contains(text, "python") {
		t.Error("CI selection, execution and validation must use Go commands")
	}
	for _, required := range []string{
		"matrix: ${{ fromJSON(needs.plan.outputs.matrix) }}",
		"CI_FULL_TESTS: ${{ vars.CI_FULL_TESTS }}",
		"CI_BASE_SHA: ${{ github.event.pull_request.base.sha }}",
		"fetch-depth: 0", "--selection test-selection.json",
		"needs.plan.outputs.mode == 'full'", "name: affected-coverage-",
		"  build-native:\n", "  lint:\n", "  build-linux-static:\n", "  build-onnx:\n", "  ci-result:\n",
		"if: always()", "go test -count=1 ./scripts/ci-result", "run: go run ./scripts/ci-result",
		"go test -count=1 ./internal/citest", "go run ./scripts/ci-select --output test-selection.json",
		"go run ./scripts/ci-test --selection test-selection.json",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("workflow is missing required wiring %q", required)
		}
	}
	events := regexp.MustCompile(`(?m)^    types: \[([^\]]+)\]`).FindStringSubmatch(text)
	if len(events) != 2 {
		t.Fatal("workflow must specify its pull request event types")
	}
	got := make(map[string]bool)
	for _, event := range strings.Split(events[1], ", ") {
		got[event] = true
	}
	want := map[string]bool{"opened": true, "synchronize": true, "reopened": true, "labeled": true, "unlabeled": true}
	if !maps.Equal(got, want) {
		t.Errorf("pull request events = %v, want %v", got, want)
	}
}

func TestCIResultCLIRejectsMissingMalformedOrNonObjectNeeds(t *testing.T) {
	for _, tc := range []struct{ name, payload, want string }{
		{"missing", "", "invalid CI_NEEDS"},
		{"malformed", "{", "invalid CI_NEEDS"},
		{"non-object", "[]", "invalid CI_NEEDS"},
		{"null", "null", "plan must be success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.payload, "pull_request", &stdout, &stderr); code == 0 {
				t.Fatal("invalid CLI input must produce a nonzero exit")
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("unexpected invalid-input response: stdout=%s stderr=%s", &stdout, &stderr)
			}
		})
	}
}
