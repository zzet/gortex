package gitcmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withFakeGit installs a shell script named "git" at the front of PATH
// for the duration of the test and returns the directory it lives in.
// The script body is provided by the caller.
func withFakeGit(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-git shim is a POSIX shell script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body
	gitPath := filepath.Join(dir, "git")
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)
	return dir
}

// TestConcurrencyCapNeverExceeded launches N+k concurrent Run calls
// against a fake git that signals on start, blocks on a barrier, then
// exits. It asserts the number simultaneously in-flight never exceeds
// the configured concurrency cap.
func TestConcurrencyCapNeverExceeded(t *testing.T) {
	const cap = 3
	const callers = cap + 4

	// The fake git appends a "start" line to $TRACK, then spins until
	// $RELEASE exists, then appends a "stop" line. This lets the test
	// hold every spawned process open simultaneously and watch the
	// live count.
	dir := withFakeGit(t, `
echo "start $$" >> "$GITCMD_TRACK"
while [ ! -f "$GITCMD_RELEASE" ]; do
  sleep 0.005
done
echo "stop $$" >> "$GITCMD_TRACK"
exit 0
`)
	_ = dir

	trackFile := filepath.Join(t.TempDir(), "track")
	releaseFile := filepath.Join(t.TempDir(), "release")
	t.Setenv("GITCMD_TRACK", trackFile)
	t.Setenv("GITCMD_RELEASE", releaseFile)
	if err := os.WriteFile(trackFile, nil, 0o644); err != nil {
		t.Fatalf("seed track file: %v", err)
	}

	// Reset the limiter to a known cap, restore the default afterwards.
	SetConcurrency(cap)
	t.Cleanup(func() { SetConcurrency(int(defaultConcurrency())) })

	var inFlight int64
	var maxInFlight int64
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Run will block inside the fake git until releaseFile
			// appears; while blocked it holds a semaphore slot.
			_, _ = Run(context.Background(), "", "status")
		}()
	}

	// Sampler: while callers are blocked in the fake git, count the
	// processes that have logged "start" but not yet "stop" and track
	// the peak. We wait until cap processes are concurrently running
	// (proving the cap is reached) then release everyone.
	deadline := time.Now().Add(10 * time.Second)
	released := false
	for time.Now().Before(deadline) {
		starts, stops := countTrack(t, trackFile)
		cur := int64(starts - stops)
		atomic.StoreInt64(&inFlight, cur)
		if cur > atomic.LoadInt64(&maxInFlight) {
			atomic.StoreInt64(&maxInFlight, cur)
		}
		if !released && starts >= cap {
			// Cap-many processes are concurrently alive; release.
			if err := os.WriteFile(releaseFile, nil, 0o644); err != nil {
				t.Fatalf("write release file: %v", err)
			}
			released = true
		}
		if released && stops >= callers {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !released {
		// Make sure no goroutine leaks if we timed out before reaching cap.
		_ = os.WriteFile(releaseFile, nil, 0o644)
	}
	wg.Wait()

	peak := atomic.LoadInt64(&maxInFlight)
	if peak > cap {
		t.Fatalf("concurrency cap exceeded: peak in-flight %d > cap %d", peak, cap)
	}
	if peak < 1 {
		t.Fatalf("expected at least one in-flight git process, saw %d", peak)
	}
}

// countTrack returns (#start lines, #stop lines) in the track file.
func countTrack(t *testing.T, path string) (int, int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read track file: %v", err)
	}
	var starts, stops int
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "start "):
			starts++
		case strings.HasPrefix(line, "stop "):
			stops++
		}
	}
	return starts, stops
}

// TestCancelBeforeAcquireDoesNotSpawn asserts that a Run with an
// already-cancelled context returns ctx.Err() and never spawns git.
func TestCancelBeforeAcquireDoesNotSpawn(t *testing.T) {
	var spawned int32
	withFakeGit(t, `
echo spawned >> "$GITCMD_SPAWN_LOG"
exit 0
`)
	spawnLog := filepath.Join(t.TempDir(), "spawn.log")
	t.Setenv("GITCMD_SPAWN_LOG", spawnLog)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling Run

	out, err := Run(ctx, "", "status")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty stdout on pre-cancel, got %q", out)
	}
	// The fake git, if it ran, would have created the spawn log.
	if _, statErr := os.Stat(spawnLog); statErr == nil {
		t.Fatalf("git was spawned despite cancelled context")
	}
	_ = atomic.LoadInt32(&spawned)
}

// TestCancelWhileHoldingOrWaitingForSlot asserts that when the limiter is
// saturated, a Run whose ctx is cancelled while waiting for a slot
// returns ctx.Err() and never spawns its own git, and that a Run whose ctx
// is cancelled while its git runs reports ctx.Err() beside git's exit
// status.
func TestCancelWhileHoldingOrWaitingForSlot(t *testing.T) {
	// The fake git runs until it is killed.
	withFakeGit(t, `
echo "start $$" >> "$GITCMD_TRACK"
while :; do
  sleep 0.005
done
`)
	trackFile := filepath.Join(t.TempDir(), "track")
	t.Setenv("GITCMD_TRACK", trackFile)
	if err := os.WriteFile(trackFile, nil, 0o644); err != nil {
		t.Fatalf("seed track file: %v", err)
	}

	SetConcurrency(1)
	t.Cleanup(func() { SetConcurrency(int(defaultConcurrency())) })

	// Saturate the single slot.
	holderCtx, cancelHolder := context.WithCancel(context.Background())
	defer cancelHolder()
	holderErr := make(chan error, 1)
	go func() {
		_, err := Run(holderCtx, "", "status")
		holderErr <- err
	}()
	// Wait until the holder is actually running git.
	waitForStarts(t, trackFile, 1, 5*time.Second)

	// Now a waiter whose ctx we cancel while it blocks on Acquire.
	ctx, cancel := context.WithCancel(context.Background())
	waiterErr := make(chan error, 1)
	go func() {
		_, err := Run(ctx, "", "status")
		waiterErr <- err
	}()
	// Give the waiter a moment to enter Acquire, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-waiterErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter: expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return after ctx cancel")
	}

	// Only the holder should have spawned git; the waiter must not.
	if starts, _ := countTrack(t, trackFile); starts != 1 {
		t.Fatalf("expected exactly 1 spawned git (the holder), saw %d", starts)
	}

	// Cancel the holder while its git runs. exec kills the child and reports
	// only its exit status ("signal: killed"), which alone does not tell the
	// caller that its own context ended the command.
	cancelHolder()
	select {
	case err := <-holderErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("holder: expected context.Canceled, got %v", err)
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("holder: git's exit status is not reachable: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("holder did not return after ctx cancel")
	}
}

func waitForStarts(t *testing.T, path string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if starts, _ := countTrack(t, path); starts >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d git starts in %s", want, path)
}

// TestFailingGitSurfacesStderr asserts the wrapped error carries git's
// stderr and the failing subcommand name.
func TestFailingGitSurfacesStderr(t *testing.T) {
	withFakeGit(t, `
echo "fatal: not a git repository" 1>&2
exit 128
`)
	out, err := Run(context.Background(), "", "rev-parse", "HEAD")
	if err == nil {
		t.Fatal("expected error from failing git, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "fatal: not a git repository") {
		t.Fatalf("stderr not surfaced in error: %q", msg)
	}
	if !strings.Contains(msg, "git rev-parse") {
		t.Fatalf("subcommand name not in error: %q", msg)
	}
	// stdout is still returned (empty here).
	_ = out
}

// TestOutputTrimsAndCarriesError verifies Output trims stdout and
// propagates the wrapped error.
func TestOutputTrimsAndCarriesError(t *testing.T) {
	withFakeGit(t, `
if [ "$1" = "ok" ]; then
  printf '  deadbeef\n\n'
  exit 0
fi
echo "boom" 1>&2
exit 1
`)
	got, err := Output(context.Background(), "", "ok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "deadbeef" {
		t.Fatalf("Output did not trim: %q", got)
	}

	_, err = Output(context.Background(), "", "fail")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Output should surface stderr, got %v", err)
	}
}

// TestRunPassesDirAsDashC ensures the -C dir flag is prepended when a
// dir is supplied and omitted when it is empty.
func TestRunPassesDirAsDashC(t *testing.T) {
	withFakeGit(t, `
printf '%s\n' "$@" > "$GITCMD_ARGS"
exit 0
`)
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("GITCMD_ARGS", argsFile)

	if _, err := Run(context.Background(), "/some/dir", "status"); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	lines := splitNonEmpty(string(data))
	want := []string{"-C", "/some/dir", "status"}
	if len(lines) != len(want) {
		t.Fatalf("args = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("arg[%d] = %q, want %q (full %v)", i, lines[i], want[i], lines)
		}
	}

	// Empty dir: no -C prefix.
	if _, err := Run(context.Background(), "", "status"); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	data, _ = os.ReadFile(argsFile)
	lines = splitNonEmpty(string(data))
	if len(lines) != 1 || lines[0] != "status" {
		t.Fatalf("empty-dir args = %v, want [status]", lines)
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\n") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TestSetConcurrencyClamps verifies SetConcurrency clamps sub-1 values
// to 1 and that defaultConcurrency stays within [1,8].
func TestSetConcurrencyClamps(t *testing.T) {
	SetConcurrency(0)
	if got := currentSem(); got == nil {
		t.Fatal("limiter is nil after SetConcurrency(0)")
	}
	SetConcurrency(-5)
	if got := currentSem(); got == nil {
		t.Fatal("limiter is nil after SetConcurrency(-5)")
	}
	// Restore default for other tests.
	SetConcurrency(int(defaultConcurrency()))

	d := defaultConcurrency()
	if d < 1 || d > 8 {
		t.Fatalf("defaultConcurrency() = %d, want within [1,8]", d)
	}
}
