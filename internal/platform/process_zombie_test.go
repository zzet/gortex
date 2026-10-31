//go:build darwin || linux

package platform

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessAliveAndExistsZombie(t *testing.T) {
	// Keep the child alive until both live-process probes have been checked.
	child := exec.Command("sh", "-c", "read value; exit 0")
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	// Wait only AFTER the zombie assertions, keeping its PID allocated throughout.
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	pid := child.Process.Pid
	identity, err := CaptureProcessIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	if !ProcessAlive(pid) || !ProcessExists(pid) {
		t.Fatal("a live child must be alive and exist")
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}

	// Observe the zombie independently of the probes under test. In particular,
	// do not call child.Wait (or use ProcessAlive to wait for the child to exit).
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			t.Fatalf("ps failed for unreaped child %d: %v", pid, err)
		}
		if strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d did not become a zombie; state %q", pid, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("unreaped zombie must still pass kill(pid, 0): %v", err)
	}
	if ProcessAlive(pid) {
		t.Error("ProcessAlive(zombie) = true, want false before Wait")
	}
	if ProcessExists(pid) {
		t.Error("ProcessExists(zombie) = true, want false before Wait")
	}
	if running, err := identity.Running(); err != nil || running {
		t.Errorf("captured zombie Running() = %v, %v; want false, nil", running, err)
	}
	if same, err := identity.Matches(); err != nil || !same {
		t.Errorf("captured zombie no longer has its native birth identity: same=%v error=%v", same, err)
	}
}
