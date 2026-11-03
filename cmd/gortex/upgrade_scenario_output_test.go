//go:build acceptance && (linux || darwin)

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Persist each run independently: t.Fatal in the runner must not lose the
// partial postmortem or either raw stream. These files are intentional evidence,
// not temporary daemon state, and are never silently removed on failure.
func scenarioCaptureOutput(t *testing.T, interrupted bool, run *upgradeScenarioRun) (*os.File, *os.File, func()) {
	t.Helper()
	prefix := os.Getenv(upgradeScenarioOutEnv)
	if prefix == "" {
		dir, err := os.MkdirTemp("", "gx-upgrade-evidence-")
		if err != nil {
			t.Fatal(err)
		}
		prefix = filepath.Join(dir, "receipt.json")
	}
	role := "restarted"
	if interrupted {
		role = "interrupted"
	}
	prefix += "." + role
	run.StdoutPath, run.StderrPath = prefix+".stdout.log", prefix+".stderr.log"
	open := func(path string) (*os.File, error) {
		return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	stdout, err := open(run.StdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := open(run.StderrPath)
	if err != nil {
		_ = stdout.Close()
		t.Fatal(err)
	}
	t.Logf("upgrade %s raw output: %s and %s", role, run.StdoutPath, run.StderrPath)
	return stdout, stderr, func() {
		for _, f := range []*os.File{stdout, stderr} {
			if err := f.Close(); err != nil {
				t.Errorf("close raw output %s: %v", f.Name(), err)
			}
		}
		blob, err := json.MarshalIndent(run, "", "  ")
		if err != nil {
			t.Errorf("encode partial run: %v", err)
			return
		}
		f, err := open(prefix + ".run.json")
		if err != nil {
			t.Errorf("create partial run: %v", err)
			return
		}
		if _, err := f.Write(append(blob, '\n')); err != nil {
			t.Errorf("write partial run: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Errorf("close partial run: %v", err)
		}
	}
}
