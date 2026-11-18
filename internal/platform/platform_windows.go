//go:build windows

package platform

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports while a
// process is still running (Win32 STILL_ACTIVE / STATUS_PENDING).
const stillActive = 259

// ShutdownSignals returns the signals a long-running process should trap
// to begin a graceful shutdown. Windows has no SIGTERM; os.Interrupt —
// delivered for Ctrl-C and Ctrl-Break — is the only portable trigger.
func ShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// ProcessAlive reports whether a process with the given PID currently
// exists and has not yet exited. It opens a query handle and inspects
// the exit code: only a still-running process reports STILL_ACTIVE.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h) //nolint:errcheck // best-effort handle close
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// ProcessExists reports whether a process with the given PID exists.
// On Windows it is the same as ProcessAlive; see the Unix version for why
// the two differ there.
func ProcessExists(pid int) bool {
	return ProcessAlive(pid)
}

// TerminateProcess asks the process to exit. Windows offers no graceful
// signal a console-less detached process can receive, so this is a hard
// TerminateProcess — the same as KillProcess. The daemon's preferred
// stop path is the control-socket RPC; this is only the fallback for a
// daemon that no longer answers the socket.
func TerminateProcess(pid int) error {
	return KillProcess(pid)
}

// KillProcess forcibly terminates the process.
func KillProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// DetachSysProcAttr returns the SysProcAttr that detaches a spawned child
// from the parent's console: a new process group, so a Ctrl-C in the
// parent console isn't forwarded, plus CREATE_NO_WINDOW so the child owns
// a console of its own that has no window.
//
// Not DETACHED_PROCESS. A console-less daemon makes Windows allocate a
// fresh, visible console for every console child that does not opt out —
// and the children that cannot opt out are the ones spawned by code we
// don't own: go/packages runs `go list` / `go env` through x/tools' own
// exec.Command, which never sets HideWindow, so a Go repository's
// enrichment popped a blank go.exe window per invocation (issue #783).
// ConfigureBackgroundCommand covers the spawns we construct ourselves;
// this covers everything underneath them, because a windowless console is
// inherited by every descendant. The two flags are mutually exclusive.
func DetachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
	}
}
