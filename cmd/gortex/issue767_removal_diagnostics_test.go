//go:build acceptance

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIssue767LiveRemovalFailureRetainsFirstEvidence(t *testing.T) {
	f := newIssue767Fixture(t, "") // Git only; no daemon or store is opened.
	f.write(f.logPath(), "watcher/view evidence before removal\n")
	args := []string{"worktree", "remove", "--force", f.linked}
	output, err := f.tryGit(f.primary, args...)
	if err == nil || !bytes.Contains(output, []byte("not a working tree")) {
		t.Fatalf("expected original Git failure, got %v: %s", err, output)
	}
	path := filepath.Join(f.root, "worktree-remove-failure.json")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failure evidence must exist before tryGit returns: %v", err)
	}
	var report struct {
		Error       string `json:"error"`
		Output      string `json:"output"`
		DaemonPID   int    `json:"daemon_pid"`
		HandleOwner string `json:"handle_owner"`
		DaemonLog   string `json:"daemon_log"`
	}
	if err := json.Unmarshal(first, &report); err != nil {
		t.Fatal(err)
	}
	if report.Error == "" || report.Output != string(output) || report.DaemonPID != 0 || report.HandleOwner != "UNKNOWN" {
		t.Fatalf("original error and unknown owner must be retained: %+v", report)
	}
	if !strings.Contains(report.DaemonLog, "watcher/view evidence before removal") {
		t.Fatalf("missing existing daemon log: %+v", report)
	}
	trace, err := os.ReadFile(filepath.Join(f.root, "worktree-remove-trace.jsonl"))
	if err != nil || !bytes.Contains(trace, []byte(`"event":"exit"`)) {
		t.Fatalf("missing completed Git Trace2 evidence: %v: %s", err, trace)
	}
	// A later failure must not overwrite the first pre-cleanup observation.
	f.write(f.logPath(), "later evidence\n")
	_, _ = f.tryGit(f.primary, args...)
	second, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("first failure evidence changed: %v", err)
	}
}

func TestIssue767SuccessfulLiveRemovalKeepsOriginalOperation(t *testing.T) {
	f := newIssue767Fixture(t, "")
	f.git(f.primary, "worktree", "add", "-b", "linked", f.linked)
	if output, err := f.tryGit(f.primary, "worktree", "remove", "--force", f.linked); err != nil {
		t.Fatalf("original removal failed: %v: %s", err, output)
	}
	if _, err := os.Stat(f.linked); !os.IsNotExist(err) {
		t.Fatalf("linked worktree was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "worktree-remove-failure.json")); !os.IsNotExist(err) {
		t.Fatalf("successful removal emitted failure evidence: %v", err)
	}
}

func TestIssue767OtherGitFailuresDoNotCollectRemovalEvidence(t *testing.T) {
	f := newIssue767Fixture(t, "")
	if _, err := f.tryGit(f.primary, "--issue767-intentional-failure"); err == nil {
		t.Fatal("expected Git failure")
	}
	if _, err := os.Stat(filepath.Join(f.root, "worktree-remove-failure.json")); !os.IsNotExist(err) {
		t.Fatalf("unrelated Git failure collected removal evidence: %v", err)
	}
}

func TestIssue767RemovalEvidenceStaysBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	data := bytes.Repeat([]byte("a"), 2*issue767RemovalEvidenceLimit)
	data = append(data, []byte("last watcher event")...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	tail, err := issue767RemovalLogTail(path)
	if err != nil || len(tail) != issue767RemovalEvidenceLimit || !strings.HasSuffix(tail, "last watcher event") {
		t.Fatalf("bounded log tail: bytes=%d err=%v", len(tail), err)
	}
	var output issue767RemovalOutput
	for _, chunk := range [][]byte{data, []byte("still drain the helper")} {
		if n, err := output.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("helper output stopped draining: bytes=%d err=%v", n, err)
		}
	}
	if !output.truncated || len(output.data) != issue767RemovalEvidenceLimit {
		t.Fatalf("helper output exceeded bound: bytes=%d truncated=%v", len(output.data), output.truncated)
	}
}
