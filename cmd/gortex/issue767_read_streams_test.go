//go:build acceptance

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestIssue767ReadCommandOutputKeepsDiagnosticsSeparate(t *testing.T) {
	if mode := os.Getenv("GORTEX_ISSUE767_STREAM_CHILD"); mode != "" {
		fmt.Fprintln(os.Stderr, "file: unknown (0 files)")
		if mode == "invalid" {
			fmt.Fprintln(os.Stdout, "not-json")
		} else {
			fmt.Fprintln(os.Stdout, `{"freshness":{"exact":true},"content":"Issue767RestartedMarker"}`)
		}
		if mode == "failure" {
			os.Exit(7)
		}
		os.Exit(0)
	}
	for _, mode := range []string{"success", "failure", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			f := &issue767Fixture{t: t, root: root, binary: os.Args[0], env: append(os.Environ(), "GORTEX_ISSUE767_STREAM_CHILD="+mode)}
			stdout, err := f.tryJSONCommand(30*time.Second, root, "-test.run=^TestIssue767ReadCommandOutputKeepsDiagnosticsSeparate$")
			if mode == "failure" {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 7 {
					t.Fatalf("exit failure changed: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			stderr, err := os.ReadFile(filepath.Join(root, "json-command-000001.stderr"))
			if err != nil || string(stderr) != "file: unknown (0 files)\n" {
				t.Fatalf("diagnostic lost: %q %v", stderr, err)
			}
			retained, err := os.ReadFile(filepath.Join(root, "json-command-000001.stdout.json"))
			if err != nil || string(retained) != string(stdout) {
				t.Fatalf("raw stdout changed: %q %v", retained, err)
			}
			var value map[string]any
			parseErr := json.Unmarshal(stdout, &value)
			if mode == "invalid" {
				if parseErr == nil {
					t.Fatal("invalid stdout became valid")
				}
			} else if parseErr != nil || value["content"] != "Issue767RestartedMarker" {
				t.Fatalf("source answer changed: %s %v", stdout, parseErr)
			}
		})
	}
}
