//go:build acceptance

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	t.Run("same_drive", func(t *testing.T) { issue767WorktreeReadiness(t, binary, false) })
	t.Run("cross_drive", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("cross-drive variant requires native Windows drive letters; same-drive acceptance still runs")
		}
		issue767WorktreeReadiness(t, binary, true)
	})
}

func issue767WorktreeReadiness(t *testing.T, binary string, crossDrive bool) {
	t.Helper()
	f := newIssue767Fixture(t, binary)
	if crossDrive {
		f.linked = issue767CrossDriveWorktree(t, f)
	}
	t.Logf("worktree placement: primary=%s linked=%s cross_drive=%t", f.primary, f.linked, crossDrive)
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
	f.readinessEdit("before-restart-dry-run", path, match, replacement, true, 45*time.Second)
	afterPreview, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterPreview) != string(before) {
		t.Fatal("edit dry run modified the working copy")
	}

	started = time.Now()
	f.readinessEdit("before-restart-real", path, match, replacement, false, 45*time.Second)
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
	beforeRestartEdit, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.root, "after-restart-before.go"), string(beforeRestartEdit))
	secondMatch, secondReplacement := replacement, "func Issue767RestartedMarker()"
	if strings.Count(string(beforeRestartEdit), secondMatch) != 1 {
		t.Fatal("after-restart fixture must contain exactly one second edit target")
	}
	f.readinessEdit("after-restart-dry-run", path, secondMatch, secondReplacement, true, 3*time.Second)
	afterRestartPreview, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.root, "after-restart-preview.go"), string(afterRestartPreview))
	if string(afterRestartPreview) != string(beforeRestartEdit) {
		t.Fatal("after-restart edit dry run modified the working copy")
	}
	f.readinessEdit("after-restart-real", path, secondMatch, secondReplacement, false, 3*time.Second)
	afterRestartEdit, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.root, "after-restart-edited.go"), string(afterRestartEdit))
	if want := strings.Replace(string(beforeRestartEdit), secondMatch, secondReplacement, 1); string(afterRestartEdit) != want {
		t.Fatalf("after-restart edit wrote unexpected source: %q", afterRestartEdit)
	}
	f.awaitSymbol(f.linked, "Issue767RestartedMarker")
	if old, err := f.trySearchSymbol(f.linked, "Issue767EditedMarker"); err != nil || old {
		t.Fatalf("after-restart replaced symbol leaked through overlay: found=%v err=%v", old, err)
	}
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
	if leaked, err := f.trySearchSymbol(f.primary, "Issue767RestartedMarker"); err != nil || leaked {
		t.Fatalf("after-restart edit leaked into primary: found=%v err=%v", leaked, err)
	}
	f.readinessRead("after-restart-linked-read", f.linked, "Issue767RestartedMarker", "Issue767PrimaryMarker")
	f.readinessRead("after-restart-primary-read", f.primary, "Issue767PrimaryMarker", "Issue767RestartedMarker")
	f.git(f.primary, "worktree", "remove", "--force", f.linked)
	f.awaitRemoved()
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
}

func (f *issue767Fixture) readinessRead(phase, root, present, absent string) {
	f.t.Helper()
	request := map[string]any{"operation": "file", "target": map[string]any{"file": filepath.Join(root, "marker.go")}}
	if root != f.primary {
		request["view"] = map[string]any{"kind": "worktree", "path": root}
	}
	payload, err := json.Marshal(request)
	if err != nil {
		f.t.Fatal(err)
	}
	output, err := f.tryJSONCommand(30*time.Second, root, "call", "read", "--index", root, "--json", string(payload), "--format", "json")
	f.write(filepath.Join(f.root, phase+".json"), string(output))
	if err != nil {
		f.t.Fatalf("%s: %v; output: %s", phase, err, output)
	}
	var response any
	if err := json.Unmarshal(output, &response); err != nil {
		f.t.Fatalf("%s is not JSON: %v", phase, err)
	}
	_, refused := issue767JSONEvidence(response, "")
	if refused || (root != f.primary && !issue767JSONExact(response)) || !strings.Contains(string(output), present) || strings.Contains(string(output), absent) {
		f.t.Fatalf("%s did not prove isolated source: %s", phase, output)
	}
}

// Keep CLI diagnostics as evidence without mixing stderr into the JSON wire.
// A successful file read may still report unknown graph freshness on stderr.
func issue767ReadCommandOutput(cmd *exec.Cmd) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// The extra volume is optional, but the reason it could not be exercised is
// explicit in the uploaded output. Only freshly allocated owned directories
// are ever changed; no junction, subst drive or system configuration is made.
func issue767CrossDriveWorktree(t *testing.T, f *issue767Fixture) string {
	t.Helper()
	primaryVolume := filepath.VolumeName(f.primary)
	var unavailable []string
	for letter := 'A'; letter <= 'Z'; letter++ {
		volume := string(letter) + ":"
		if strings.EqualFold(volume, primaryVolume) {
			continue
		}
		root := volume + string(filepath.Separator)
		if !issue767FixedDrive(root) {
			continue // do not allocate fixtures on removable or mapped network drives
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		owned, err := os.MkdirTemp(root, "gx767-")
		if err != nil {
			unavailable = append(unavailable, fmt.Sprintf("%s: %v", volume, err))
			continue
		}
		t.Cleanup(func() {
			// Stop the private child before removing its observed worktree,
			// including when a failed assertion is unwinding.
			f.stop()
			if os.Getenv("GORTEX_ISSUE767_ARTIFACT_DIR") == "" {
				if err := os.RemoveAll(owned); err != nil {
					t.Errorf("remove owned cross-drive fixture %s: %v", owned, err)
				}
			}
		})
		return filepath.Join(owned, "linked")
	}
	if len(unavailable) == 0 {
		unavailable = append(unavailable, "no other accessible fixed local drive root exists")
	}
	t.Skipf("cross-drive variant unavailable: primary volume=%q; %s", primaryVolume, strings.Join(unavailable, "; "))
	return ""
}

func (f *issue767Fixture) readinessEdit(phase, path, match, replacement string, dryRun bool, limit time.Duration) {
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
	ctx, cancel := context.WithTimeout(f.t.Context(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.binary, "call", "edit", "--index", f.linked, "--json", string(payload), "--format", "json")
	cmd.Dir, cmd.Env = f.linked, f.env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	started := time.Now()
	output, err := cmd.Output()
	elapsed := time.Since(started)
	f.write(filepath.Join(f.root, phase+".stdout.json"), string(output))
	f.write(filepath.Join(f.root, phase+".stderr.log"), stderr.String())
	refused := err != nil || issue767ExactEditResponse(output) != nil
	f.t.Logf("worktree edit admission phase=%s dry_run=%t elapsed=%s limit=%s refused=%t command_error=%v", phase, dryRun, elapsed, limit, refused, err)
	if err := issue767EditAdmissionResult(output, err, elapsed, limit); err != nil {
		f.t.Fatalf("%s: %v\nstderr: %s", phase, err, stderr.String())
	}
}

func issue767EditAdmissionResult(output []byte, commandErr error, elapsed, limit time.Duration) error {
	if commandErr != nil {
		return fmt.Errorf("isolated edit command: %w; stdout: %s", commandErr, output)
	}
	if err := issue767ExactEditResponse(output); err != nil {
		return err
	}
	if elapsed > limit {
		return fmt.Errorf("edit admission took %s, exceeding %s", elapsed, limit)
	}
	return nil
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
		{"read_only_busy", `{"freshness":{"exact":true},"error_code":"view_read_only","message":"checkout mutation lane is busy"}`, true},
		{"invalid", "not json", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := issue767ExactEditResponse([]byte(tc.response)); (err != nil) != tc.wantError {
				t.Fatalf("err=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestIssue767EditAdmissionRequiresTimelyExactSuccess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		elapsed  time.Duration
		err      error
		wantErr  bool
	}{
		{"at_limit", `{"freshness":{"exact":true},"success":true}`, 3 * time.Second, nil, false},
		{"late_success", `{"freshness":{"exact":true},"success":true}`, 3*time.Second + time.Nanosecond, nil, true},
		{"deadline_after_possible_commit", `{"freshness":{"exact":true},"success":true}`, 3 * time.Second, context.DeadlineExceeded, true},
		{"fast_refusal", `{"error_code":"view_read_only"}`, time.Millisecond, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := issue767EditAdmissionResult([]byte(tc.response), tc.err, tc.elapsed, 3*time.Second); (err != nil) != tc.wantErr {
				t.Fatalf("result=%v; wantErr=%t", err, tc.wantErr)
			}
		})
	}
}
