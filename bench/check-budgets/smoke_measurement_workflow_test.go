package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSmokeMeasurementWorkflowReservesSetupCleanupAndEvidence(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/perf-gate.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Timeout    int  `yaml:"timeout-minutes"`
			ReportOnly bool `yaml:"continue-on-error"`
			Steps      []struct {
				Name    string `yaml:"name"`
				Uses    string `yaml:"uses"`
				Timeout int    `yaml:"timeout-minutes"`
				If      string `yaml:"if"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["smoke"]
	if !job.ReportOnly {
		t.Fatal("measurement must remain unconditionally report-only")
	}
	if job.Timeout != 145 {
		t.Errorf("measurement job safety bound=%dm, want145m including bounded setup/cleanup/upload", job.Timeout)
	}
	setup, measurement, upload := 0, 0, 0
	for _, step := range job.Steps {
		want := 0
		switch {
		case strings.HasPrefix(step.Uses, "actions/checkout@"):
			want = 2
		case strings.HasPrefix(step.Uses, "actions/setup-go@"):
			want = 5
		case step.Name == "Build clean candidate and owned measurement commands":
			want = 5
		case step.Name == "Retrieve the exact main pinned at PR planning time":
			want = 2
		case step.Name == "Prepare the reviewed contemporary fixture":
			want = 2
		case step.Name == "Compile all three clean candidate benchmark executables":
			want = 4
		case step.Name == "Run five standalone steps and same-job microbenchmark comparison":
			want = 122
			measurement = step.Timeout
		case step.Name == "Preserve attempted smoke reports and failures":
			want = 3
			upload = step.Timeout
			if step.If != "always()" {
				t.Error("failed measurement must preserve evidence")
			}
		default:
			t.Errorf("unbudgeted smoke step %q", step.Name)
		}
		if step.Timeout != want {
			t.Errorf("step%q safety bound=%dm, want%dm", step.Name, step.Timeout, want)
		}
		if want > 0 && want < 100 && step.Name != "Preserve attempted smoke reports and failures" {
			setup += step.Timeout
		}
	}
	if setup != 20 || measurement != 122 || upload != 3 || setup+measurement+upload > job.Timeout {
		t.Errorf("insufficient measurement/cleanup reserve: setup=%d measure+cleanup=%d upload=%d job=%d", setup, measurement, upload, job.Timeout)
	}
}
