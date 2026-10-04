//go:build !windows

package gitcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIndexRefreshCancellationLetsGitRemoveItsOwnLock(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git unavailable")
	}
	repo := t.TempDir()
	git := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, realGit, append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return out
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	git("init", "-q")
	git("config", "gc.auto", "0")
	git("config", "maintenance.auto", "0")
	body := []byte(strings.Repeat("owned-refresh-content\n", 49933))
	for i := range 64 {
		if err := os.WriteFile(filepath.Join(repo, fmt.Sprintf("file-%03d.txt", i)), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	stageBefore := git("ls-files", "--stage", "-z")
	past := time.Now().Add(-time.Hour)
	for i := range 64 {
		if err := os.Chtimes(filepath.Join(repo, fmt.Sprintf("file-%03d.txt", i)), past, past); err != nil {
			t.Fatal(err)
		}
	}
	lock := filepath.Join(repo, ".git", "index.lock")
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preexisting lock: %v", err)
	}
	pidPath := filepath.Join(t.TempDir(), "pid")
	t.Setenv("GITCMD_REFRESH_PID", pidPath)
	tracePath := filepath.Join(t.TempDir(), "refresh-trace.jsonl")
	t.Setenv("GIT_TRACE2_EVENT", tracePath)
	// exec preserves the wrapper's PID; the sole writer is the real Git child.
	withFakeGit(t, "echo $$ > \"$GITCMD_REFRESH_PID\"\nexec '"+strings.ReplaceAll(realGit, "'", "'\\''")+"' \"$@\"\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancelIssued := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := run(ctx, repo, nil, func(cmd *exec.Cmd) {
			configureIndexRefreshCancellation(cmd)
			actualCancel := cmd.Cancel
			cmd.Cancel = func() error { err := actualCancel(); close(cancelIssued); return err }
		}, "update-index", "-q", "--refresh")
		done <- err
	}()
	pid, joined := 0, false
	t.Cleanup(func() {
		// The lock is created before Git installs its tempfile signal handler.
		// Actual refresh entry follows that registration; lock existence alone
		// could pause Git in the unprotected open-to-registration window.
		refreshEntered := false
		trace, _ := os.ReadFile(tracePath)
		for _, line := range bytes.Split(trace, []byte{'\n'}) {
			var event struct {
				Event    string `json:"event"`
				Category string `json:"category"`
				Label    string `json:"label"`
			}
			if json.Unmarshal(line, &event) == nil && event.Event == "region_enter" && event.Category == "index" && event.Label == "refresh" {
				refreshEntered = true
				break
			}
		}
		if pid > 0 && refreshEntered {
			_ = syscall.Kill(pid, syscall.SIGCONT)
		}
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				if pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("owned refresh not joined")
				}
			}
		}
	})
	end := time.Now().Add(5 * time.Second)
	qualified := false
	for time.Now().Before(end) {
		data, readErr := os.ReadFile(pidPath)
		if readErr == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if pid > 0 {
			if _, err := os.Lstat(lock); err == nil {
				if err := syscall.Kill(pid, 0); err != nil {
					t.Fatalf("lock without live owned Git: %v", err)
				}
				if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(lock); err != nil {
					t.Fatalf("lock vanished before pause: %v", err)
				}
				qualified = true
				break
			}
		}
		select {
		case err := <-done:
			joined = true
			t.Fatalf("refresh ended before lock witness: %v", err)
		default:
		}
		time.Sleep(500 * time.Microsecond)
	}
	if !qualified {
		t.Fatal("no positively witnessed live Git refresh with new lock")
	}
	started := time.Now()
	cancel()
	select {
	case <-cancelIssued:
	case <-time.After(time.Second):
		t.Fatal("cancellation action not issued")
	}
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		joined = true
		if err == nil {
			t.Fatal("cancelled refresh reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled refresh did not join within its 2s grace")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("owned child still present: %v", err)
	}
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Git did not remove its own lock: %v", err)
	}
	// A new real write succeeds immediately without retrying or deleting locks.
	git("update-index", "-q", "--refresh")
	if got := git("ls-files", "--stage", "-z"); !bytes.Equal(got, stageBefore) {
		t.Fatal("refresh changed staged object identity")
	}
	for i := range 64 {
		got, err := os.ReadFile(filepath.Join(repo, fmt.Sprintf("file-%03d.txt", i)))
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("refresh changed source %d: %v", i, err)
		}
	}
	t.Logf("real owned Git lock cleaned after cancellation in %s; child absent, 64 payloads and staged IDs unchanged", time.Since(started))
}

func TestIndexRefreshGraceIsBoundedWhenChildIgnoresTerminate(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("GITCMD_REFRESH_READY", ready)
	withFakeGit(t, "trap '' TERM\necho ready > \"$GITCMD_REFRESH_READY\"\nwhile :; do :; done\n")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := RunIndexRefresh(ctx, "", "update-index", "-q", "--refresh"); done <- err }()
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("fallback child not joined")
			}
		}
	})
	end := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("owned child never ready")
		}
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		joined = true
		if err == nil {
			t.Fatal("forced fallback reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("2s force fallback did not join")
	}
	elapsed := time.Since(started)
	if elapsed < 2*time.Second || elapsed > 3*time.Second {
		t.Fatalf("unexpected grace %s", elapsed)
	}
}
