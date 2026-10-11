//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIssue767WorktreeReadinessIntegration exercises the agent workflow against
// an explicitly supplied binary in a private daemon, store, and Git fixture.
// It never connects to or restarts the user's daemon. Admission under an occupied
// build gate is covered by deterministic coordinator tests, not wall-clock races
// between child processes here.
func TestIssue767WorktreeReadinessIntegration(t *testing.T) {
	binary := os.Getenv("GORTEX_ISSUE767_READINESS_BINARY")
	if binary == "" {
		t.Skip("set GORTEX_ISSUE767_READINESS_BINARY for isolated worktree validation")
	}
	if deadline, ok := t.Deadline(); ok && time.Until(deadline) < 6*time.Minute {
		t.Fatal("use go test -timeout 10m or longer for isolated worktree validation")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	f := newIssue767Fixture(t, binary)
	f.start()
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")

	// A clean checkout at the same HEAD is automatically discovered. No track
	// call or tracking-config edit is permitted for this linked checkout.
	started := time.Now()
	f.git(f.primary, "worktree", "add", "-b", "issue767-readiness", f.linked)
	f.awaitSymbol(f.linked, "Issue767PrimaryMarker")
	t.Logf("new clean worktree to exact search: %s", time.Since(started))
	path := filepath.Join(f.linked, "marker.go")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	match := "func Issue767PrimaryMarker()"
	replacement := "func Issue767EditedMarker()"
	if strings.Count(string(before), match) != 1 {
		t.Fatal("fixture must contain exactly one edit target")
	}
	f.readinessEdit(path, match, replacement, true)
	afterPreview, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterPreview) != string(before) {
		t.Fatal("edit dry run modified the working copy")
	}

	started = time.Now()
	f.readinessEdit(path, match, replacement, false)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(string(before), match, replacement, 1); string(after) != want {
		t.Fatalf("edit wrote unexpected source: %q", after)
	}
	f.awaitSymbol(f.linked, "Issue767EditedMarker")
	t.Logf("worktree edit to exact refreshed search: %s", time.Since(started))
	if old, err := f.trySearchSymbol(f.linked, "Issue767PrimaryMarker"); err != nil || old {
		t.Fatalf("replaced base symbol leaked through overlay: found=%v err=%v", old, err)
	}
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
	if leaked, err := f.trySearchSymbol(f.primary, "Issue767EditedMarker"); err != nil || leaked {
		t.Fatalf("worktree edit leaked into primary: found=%v err=%v", leaked, err)
	}

	// Restart only the captured private child, then recheck exact dirty-view
	// routing and primary isolation before exercising automatic removal cleanup.
	f.stop()
	started = time.Now()
	f.start()
	f.awaitSymbol(f.linked, "Issue767EditedMarker")
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
	t.Logf("private warm restart to exact worktree search: %s", time.Since(started))
	f.git(f.primary, "worktree", "remove", "--force", f.linked)
	f.awaitRemoved()
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
}

func (f *issue767Fixture) readinessEdit(path, match, replacement string, dryRun bool) {
	f.t.Helper()
	payload, err := json.Marshal(map[string]any{
		"operation": "file",
		"target":    map[string]any{"file": path},
		"match":     match, "replacement": replacement, "dry_run": dryRun,
		"view": map[string]any{"kind": "worktree", "path": f.linked},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	// Never retry a mutation automatically: a timed-out response may have
	// committed bytes. A failed call leaves its private fixture logs as evidence.
	ctx, cancel := context.WithTimeout(f.t.Context(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.binary, "call", "edit", "--index", f.linked, "--json", string(payload), "--format", "json")
	cmd.Dir, cmd.Env = f.linked, f.env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("isolated edit command: %v\nstdout: %s\nstderr: %s", err, output, stderr.String())
	}
	if err := issue767ExactEditResponse(output); err != nil {
		f.t.Fatalf("worktree edit dry_run=%v: %v", dryRun, err)
	}
}

func issue767ExactEditResponse(output []byte) error {
	var value any
	if err := json.Unmarshal(output, &value); err != nil {
		return fmt.Errorf("edit response is not JSON: %w: %s", err, output)
	}
	_, refused := issue767JSONEvidence(value, "")
	if refused || !issue767JSONExact(value) {
		return fmt.Errorf("edit response did not prove exact non-error view: %s", output)
	}
	return nil
}

func TestIssue767ExactEditResponse(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		wantError      bool
	}{
		{"exact", `{"freshness":{"exact":true},"success":true}`, false},
		{"fallback", `{"freshness":{"exact":false},"success":true}`, true},
		{"unlabelled", `{"success":true}`, true},
		{"error", `{"freshness":{"exact":true},"isError":true}`, true},
		{"view_building", `{"error_code":"view_building"}`, true},
		{"invalid", "not json", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := issue767ExactEditResponse([]byte(tc.response)); (err != nil) != tc.wantError {
				t.Fatalf("err=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}
