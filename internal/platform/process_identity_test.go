//go:build darwin || linux || windows

package platform

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestProcessIdentityChild(t *testing.T) {
	if os.Getenv("GORTEX_TEST_PROCESS_IDENTITY_CHILD") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func identityChild(t *testing.T) (ProcessIdentity, func() error, func() error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessIdentityChild$")
	cmd.Env = append(os.Environ(), "GORTEX_TEST_PROCESS_IDENTITY_CHILD=1", "GORACE=atexit_sleep_ms=0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	var once sync.Once
	var waitErr error
	wait := func() error { once.Do(func() { waitErr = cmd.Wait() }); return waitErr }
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child did not enter its owned loop: line=%q error=%v", line, err)
	}
	identity, err := CaptureProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if running, err := identity.Running(); err != nil || !running {
		t.Fatalf("ready child must have captured running state: running=%v error=%v", running, err)
	}
	return identity, stdin.Close, wait
}

func TestProcessIdentityRefusesUnknownAndChangedIncarnations(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if _, err := CaptureProcessIdentity(pid); err == nil {
			t.Fatalf("accepted invalid PID %d", pid)
		}
	}
	if err := (ProcessIdentity{}).Kill(); err == nil {
		t.Fatal("signaled an unknown identity")
	}
	if err := (ProcessIdentity{}).DiagnosticTerminate(); err == nil {
		t.Fatal("sent diagnostic termination to an unknown identity")
	}
	child, closeInput, wait := identityChild(t)
	if child.PID() <= 0 {
		t.Fatal("lost captured PID")
	}
	token, err := child.Token()
	if err != nil || token == "" {
		t.Fatalf("actual child token=%q error=%v", token, err)
	}
	again, err := CaptureProcessIdentity(child.PID())
	if err != nil {
		t.Fatal(err)
	}
	stableToken, err := again.Token()
	if err != nil || stableToken != token {
		t.Fatalf("same native child token changed: %q -> %q (%v)", token, stableToken, err)
	}
	changed := child
	changed.birth[0]++ // Simulate a captured PID that now names another birth.
	changedToken, err := changed.Token()
	if err != nil || changedToken == token {
		t.Fatalf("different native birth did not change token: %q (%v)", changedToken, err)
	}
	if same, err := changed.Matches(); err != nil || same {
		t.Fatalf("changed identity match=%v error=%v", same, err)
	}
	if running, err := changed.Running(); running || !errors.Is(err, ErrProcessIdentityChanged) {
		t.Fatalf("changed incarnation must remain a state error: running=%v error=%v", running, err)
	}
	for _, signal := range []func() error{changed.Terminate, changed.Kill, changed.DiagnosticTerminate} {
		if err := signal(); err == nil {
			t.Fatal("accepted signal for another incarnation")
		}
		if same, err := child.Matches(); err != nil || !same {
			t.Fatalf("refused signal disturbed real child: match=%v error=%v", same, err)
		}
	}
	if err := closeInput(); err != nil {
		t.Fatal(err)
	}
	if err := wait(); err != nil {
		t.Fatalf("owned child did not exit normally: %v", err)
	}
	if same, _ := child.Matches(); same {
		t.Fatal("reaped child still matches its captured identity")
	}
	if running, err := child.Running(); running || !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("reaped child must have explicit exited evidence: running=%v error=%v", running, err)
	}
}

func TestProcessIdentitySignalsOnlyItsActualOwnedChild(t *testing.T) {
	unrelated, closeUnrelated, waitUnrelated := identityChild(t)
	for _, force := range []bool{false, true} {
		child, _, wait := identityChild(t)
		var err error
		if force {
			err = child.Kill()
		} else {
			err = child.Terminate()
		}
		if err != nil {
			t.Fatalf("signal force=%v: %v", force, err)
		}
		if err := wait(); err == nil {
			t.Fatal("terminated child unexpectedly exited successfully")
		}
		if same, _ := child.Matches(); same {
			t.Fatal("signaled child was not actually reaped")
		}
		if same, err := unrelated.Matches(); err != nil || !same {
			t.Fatalf("unrelated child changed: match=%v error=%v", same, err)
		}
	}
	if err := closeUnrelated(); err != nil {
		t.Fatal(err)
	}
	if err := waitUnrelated(); err != nil {
		t.Fatal(err)
	}
}
