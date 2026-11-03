//go:build acceptance

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

func TestIssue767ReadCommandOutputKeepsDiagnosticsSeparate(t *testing.T) {
	if mode := os.Getenv("GORTEX_ISSUE767_STREAM_CHILD"); mode != "" {
		fmt.Fprintln(os.Stderr, "file: unknown (0 files)")
		fmt.Fprintln(os.Stdout, `{"freshness":{"exact":true},"content":"Issue767RestartedMarker"}`)
		if mode == "failure" {
			os.Exit(7)
		}
		os.Exit(0)
	}
	for _, mode := range []string{"success", "failure"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestIssue767ReadCommandOutputKeepsDiagnosticsSeparate$")
			cmd.Env = append(os.Environ(), "GORTEX_ISSUE767_STREAM_CHILD="+mode)
			stdout, stderr, err := issue767ReadCommandOutput(cmd)
			if (err != nil) != (mode == "failure") {
				t.Fatalf("command failure was changed: %v", err)
			}
			if string(stderr) != "file: unknown (0 files)\n" {
				t.Fatalf("diagnostic was lost: %q", stderr)
			}
			var value map[string]any
			if err := json.Unmarshal(stdout, &value); err != nil {
				t.Fatalf("stdout is not JSON: %v: %q", err, stdout)
			}
			if value["content"] != "Issue767RestartedMarker" {
				t.Fatalf("source answer changed: %s", stdout)
			}
		})
	}
}
